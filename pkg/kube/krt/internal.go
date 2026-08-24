// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package krt

import (
	"fmt"
	"reflect"

	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/type/v1beta1"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/ptr"
)

// registerHandlerAsBatched is a helper to register the provided handler as a batched handler. This allows collections to
// only implement RegisterBatch.
func registerHandlerAsBatched[T any](c internalCollection[T], f func(o Event[T])) HandlerRegistration {
	return c.RegisterBatch(func(events []Event[T]) {
		for _, o := range events {
			f(o)
		}
	}, true)
}

// castEvent converts an Event[I] to Event[O].
// Caller is responsible for making sure these can be type converted.
// Typically this is converting to or from `any`.
func castEvent[I, O any](o Event[I]) Event[O] {
	e := Event[O]{
		Event: o.Event,
	}
	if o.Old != nil {
		e.Old = ptr.Of(any(*o.Old).(O))
	}
	if o.New != nil {
		e.New = ptr.Of(any(*o.New).(O))
	}
	return e
}

func GetStop(opts ...CollectionOption) <-chan struct{} {
	o := buildCollectionOptions(opts...)
	return o.stop
}

func buildCollectionOptions(opts ...CollectionOption) collectionOptions {
	c := &collectionOptions{}
	for _, o := range opts {
		o(c)
	}
	c.stopProvided = c.stop != nil
	if c.stop == nil {
		// TODO: https://github.com/istio/istio/issues/58791
		// if EnableAssertions {
		// 	panic("collection " + c.name + " created with no stop channel provided; will likely cause goroutine leaks")
		// }
		if len(opts) > 0 {
			log.Debugf("collection %s did not have a stop channel in opts, creating a default which may cause goroutines to leak", c.name)
		} else {
			log.Debugf("collection was created with no opts, creating a default stop channel which may cause goroutines to leak")
		}
		c.stop = make(chan struct{})
	}
	return *c
}

// collectionOptions tracks options for a collection
type collectionOptions struct {
	name         string
	augmentation func(o any) any
	stop         <-chan struct{}
	// stopProvided reports whether the caller explicitly supplied a stop channel (via WithStop).
	// When false, `stop` is a manufactured default that is never closed, so any goroutine waiting
	// on it would leak. Lifecycle goroutines (e.g. debugger unregistration) must not be started in
	// that case.
	stopProvided  bool
	debugger      *DebugHandler
	joinUnchecked bool

	// equals holds the function provided via WithEquals, if any. It is type-erased since
	// collectionOptions is not generic; it is asserted back to func(a, b O) bool at
	// collection construction.
	equals any

	indexCollectionFromString func(string) any
	metadata                  Metadata
}

type indexedDependency struct {
	id  collectionUID
	key string
	typ indexedDependencyType
}

// dependency is a specific thing that can be depended on
type dependency struct {
	id             collectionUID
	collectionName string
	// Filter over the collection
	filter *filter
}

type erasedEventHandler = func(o []Event[any])

// registerDependency is an internal interface for things that can register dependencies.
// This is called from Fetch to Collections, generally.
type registerDependency interface {
	// Registers a dependency, returning true if it is finalized
	registerDependency(*dependency, Syncer, func(f erasedEventHandler) HandlerRegistration)
	name() string
}

// getLabels returns the labels for an object, if possible.
// Warning: this will panic if the labels is not available.
func getLabels(a any) map[string]string {
	al, ok := a.(Labeler)
	if ok {
		return al.GetLabels()
	}
	pal, ok := any(&a).(Labeler)
	if ok {
		return pal.GetLabels()
	}
	ak, ok := a.(metav1.Object)
	if ok {
		return ak.GetLabels()
	}
	ac, ok := a.(config.Config)
	if ok {
		return ac.Labels
	}
	panic(fmt.Sprintf("No Labels, got %T", a))
}

// getLabelSelector returns the labels for an object, if possible.
// Warning: this will panic if the labelSelectors is not available.
func getLabelSelector(a any) map[string]string {
	ak, ok := a.(LabelSelectorer)
	if ok {
		return ak.GetLabelSelector()
	}
	val := reflect.ValueOf(a)

	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}

	specField := val.FieldByName("Spec")
	if !specField.IsValid() {
		panic(fmt.Sprintf("obj %T has no Spec", a))
	}

	labelsField := specField.FieldByName("Selector")
	if !labelsField.IsValid() {
		panic(fmt.Sprintf("obj %T has no Selector", a))
	}

	switch s := labelsField.Interface().(type) {
	case *v1beta1.WorkloadSelector:
		return s.GetMatchLabels()
	case map[string]string:
		return s
	default:
		panic(fmt.Sprintf("obj %T has unknown Selector", s))
	}
}

// Equal checks if two objects are equal. This is done through a variety of different methods, depending on the input type.
func Equal[O any](a, b O) bool {
	if ak, ok := any(a).(Equaler[O]); ok {
		return ak.Equals(b)
	}
	if ak, ok := any(a).(Equaler[*O]); ok {
		return ak.Equals(&b)
	}
	if pk, ok := any(&a).(Equaler[O]); ok {
		return pk.Equals(b)
	}
	if pk, ok := any(&a).(Equaler[*O]); ok {
		return pk.Equals(&b)
	}

	// Future improvement: add a default Kubernetes object implementation
	// ResourceVersion is tempting but probably not safe. If we are comparing objects from the API server its fine,
	// but often we will be operating on types generated by the controller itself.
	// We should have a way to opt-in to RV comparison, but not default to it.

	ap, ok := any(a).(proto.Message)
	if ok {
		if reflect.TypeOf(ap.ProtoReflect().Interface()) == reflect.TypeOf(ap) {
			return proto.Equal(ap, any(b).(proto.Message))
		}
		// If not, this is an embedded proto most likely... Sneaky.
		// DeepEqual on proto is broken, so fail fast to avoid subtle errors.
		panic(fmt.Sprintf("unable to compare object %T; perhaps it is embedding a protobuf? Provide an Equaler implementation", a))
	}

	return reflect.DeepEqual(a, b)
}

// resolveEquals resolves the comparison function for O once, so collections do not pay for
// Equal's per-call probing (and its boxing allocations) on every comparison. The probes mirror
// Equal's, in the same order; a type can have only one Equals method, so the resolved function
// always invokes the same method Equal would.
func resolveEquals[O any]() func(a, b O) bool {
	if reflect.TypeFor[O]().Kind() == reflect.Interface {
		// For interface elements the comparison depends on each element's dynamic type, which can
		// differ per call, so it cannot be resolved up front.
		return Equal[O]
	}
	var zero O
	if _, ok := any(zero).(Equaler[O]); ok {
		return func(a, b O) bool { return any(a).(Equaler[O]).Equals(b) }
	}
	if _, ok := any(zero).(Equaler[*O]); ok {
		return func(a, b O) bool { return any(a).(Equaler[*O]).Equals(&b) }
	}
	if _, ok := any(&zero).(Equaler[O]); ok {
		return func(a, b O) bool { return any(&a).(Equaler[O]).Equals(b) }
	}
	if _, ok := any(&zero).(Equaler[*O]); ok {
		return func(a, b O) bool { return any(&a).(Equaler[*O]).Equals(&b) }
	}
	if ap, ok := any(zero).(proto.Message); ok {
		if reflect.TypeOf(ap.ProtoReflect().Interface()) == reflect.TypeOf(ap) {
			return func(a, b O) bool { return proto.Equal(any(a).(proto.Message), any(b).(proto.Message)) }
		}
		// If not, this is an embedded proto most likely... Sneaky.
		// DeepEqual on proto is broken, so fail fast to avoid subtle errors.
		// Panic at comparison time rather than here, matching Equal.
		return func(a, b O) bool {
			panic(fmt.Sprintf("unable to compare object %T; perhaps it is embedding a protobuf? Provide an Equaler implementation", a))
		}
	}
	return func(a, b O) bool { return reflect.DeepEqual(a, b) }
}

// equalsForCollection returns the comparison function for a collection's elements: the explicit
// function provided via WithEquals if set, otherwise one resolved from the element type.
func equalsForCollection[O any](o collectionOptions) func(a, b O) bool {
	if o.equals != nil {
		fn, ok := o.equals.(func(a, b O) bool)
		if !ok {
			panic(fmt.Sprintf("collection %q: WithEquals function is %T, want func(a, b %v) bool", o.name, o.equals, ptr.TypeName[O]()))
		}
		return fn
	}
	return resolveEquals[O]()
}

type collectionUID uint64

var globalUIDCounter = atomic.NewUint64(1)

func nextUID() collectionUID {
	return collectionUID(globalUIDCounter.Inc())
}
