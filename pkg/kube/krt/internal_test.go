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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/type/v1beta1"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/test/util/assert"
)

// The equals* types below cover each receiver/argument shape Equal probes for.
// Each Equals implementation only compares A, so a result that also depends on B
// proves the comparison fell through to DeepEqual instead of the method.

type equalsValueValue struct{ A, B string }

func (e equalsValueValue) Equals(o equalsValueValue) bool { return e.A == o.A }

type equalsValuePointer struct{ A, B string }

func (e equalsValuePointer) Equals(o *equalsValuePointer) bool { return e.A == o.A }

type equalsPointerValue struct{ A, B string }

func (e *equalsPointerValue) Equals(o equalsPointerValue) bool { return e.A == o.A }

type equalsPointerPointer struct{ A, B string }

func (e *equalsPointerPointer) Equals(o *equalsPointerPointer) bool { return e.A == o.A }

type equalsPlain struct{ A, B string }

type equalsIface interface{ Name() string }

type equalsIfaceImpl struct{ A, B string }

func (e equalsIfaceImpl) Name() string { return e.A }

func (e equalsIfaceImpl) Equals(o equalsIface) bool { return e.A == o.Name() }

type equalsEmbedsProto struct {
	*v1beta1.WorkloadSelector
	Extra string
}

type equalsProviderOnly struct{ A, B string }

func (equalsProviderOnly) EqualsFunc() func(a, b equalsProviderOnly) bool {
	return func(a, b equalsProviderOnly) bool { return a.A == b.A }
}

type equalsProviderPointerReceiver struct{ A, B string }

func (*equalsProviderPointerReceiver) EqualsFunc() func(a, b equalsProviderPointerReceiver) bool {
	return func(a, b equalsProviderPointerReceiver) bool { return a.A == b.A }
}

type equalsProviderPointerElement struct{ A, B string }

func (*equalsProviderPointerElement) EqualsFunc() func(a, b *equalsProviderPointerElement) bool {
	return func(a, b *equalsProviderPointerElement) bool { return a.A == b.A }
}

type equalsProviderAndEqualer struct{ A, B string }

func (e equalsProviderAndEqualer) Equals(o equalsProviderAndEqualer) bool { return e.A == o.A }

func (equalsProviderAndEqualer) EqualsFunc() func(a, b equalsProviderAndEqualer) bool {
	return equalsProviderAndEqualer.Equals
}

type equalsProviderNil struct{ A string }

func (equalsProviderNil) EqualsFunc() func(a, b equalsProviderNil) bool { return nil }

func testResolveEqualsParity[O any](t *testing.T, name string, a, b O, want bool) {
	t.Helper()
	assert.Equal(t, Equal(a, b), want, name+": Equal")
	assert.Equal(t, resolveEquals[O]()(a, b), want, name+": resolveEquals")
}

func TestResolveEquals(t *testing.T) {
	// Each Equals only compares A, so "same A, different B" returning true proves the
	// resolved function dispatches to the method, exactly as Equal does.
	testResolveEqualsParity(t, "value/value", equalsValueValue{"x", "1"}, equalsValueValue{"x", "2"}, true)
	testResolveEqualsParity(t, "value/value unequal", equalsValueValue{"x", "1"}, equalsValueValue{"y", "1"}, false)
	testResolveEqualsParity(t, "value/pointer", equalsValuePointer{"x", "1"}, equalsValuePointer{"x", "2"}, true)
	testResolveEqualsParity(t, "value/pointer unequal", equalsValuePointer{"x", "1"}, equalsValuePointer{"y", "1"}, false)
	testResolveEqualsParity(t, "pointer/value", equalsPointerValue{"x", "1"}, equalsPointerValue{"x", "2"}, true)
	testResolveEqualsParity(t, "pointer/value unequal", equalsPointerValue{"x", "1"}, equalsPointerValue{"y", "1"}, false)
	testResolveEqualsParity(t, "pointer/pointer", equalsPointerPointer{"x", "1"}, equalsPointerPointer{"x", "2"}, true)
	testResolveEqualsParity(t, "pointer/pointer unequal", equalsPointerPointer{"x", "1"}, equalsPointerPointer{"y", "1"}, false)

	// Pointer element types.
	testResolveEqualsParity(t, "pointer element", &equalsPointerPointer{"x", "1"}, &equalsPointerPointer{"x", "2"}, true)
	testResolveEqualsParity(t, "pointer element unequal", &equalsPointerPointer{"x", "1"}, &equalsPointerPointer{"y", "1"}, false)

	// No Equals implementation: DeepEqual, which does compare B.
	testResolveEqualsParity(t, "deepequal", equalsPlain{"x", "1"}, equalsPlain{"x", "1"}, true)
	testResolveEqualsParity(t, "deepequal unequal", equalsPlain{"x", "1"}, equalsPlain{"x", "2"}, false)

	// Proto messages compare with proto.Equal.
	testResolveEqualsParity(t, "proto",
		&v1beta1.WorkloadSelector{MatchLabels: map[string]string{"a": "b"}},
		&v1beta1.WorkloadSelector{MatchLabels: map[string]string{"a": "b"}}, true)
	testResolveEqualsParity(t, "proto unequal",
		&v1beta1.WorkloadSelector{MatchLabels: map[string]string{"a": "b"}},
		&v1beta1.WorkloadSelector{MatchLabels: map[string]string{"a": "c"}}, false)

	// Interface elements resolve per call based on the dynamic type; the method must still win
	// over DeepEqual.
	testResolveEqualsParity[equalsIface](t, "interface", equalsIfaceImpl{"x", "1"}, equalsIfaceImpl{"x", "2"}, true)
	testResolveEqualsParity[equalsIface](t, "interface unequal", equalsIfaceImpl{"x", "1"}, equalsIfaceImpl{"y", "1"}, false)

	// EqualerProvider: the type's provided function must win over DeepEqual, in both the resolved
	// and probing paths, for every receiver/element shape.
	testResolveEqualsParity(t, "provider", equalsProviderOnly{"x", "1"}, equalsProviderOnly{"x", "2"}, true)
	testResolveEqualsParity(t, "provider unequal", equalsProviderOnly{"x", "1"}, equalsProviderOnly{"y", "1"}, false)
	testResolveEqualsParity(t, "provider pointer receiver",
		equalsProviderPointerReceiver{"x", "1"}, equalsProviderPointerReceiver{"x", "2"}, true)
	testResolveEqualsParity(t, "provider pointer element",
		&equalsProviderPointerElement{"x", "1"}, &equalsProviderPointerElement{"x", "2"}, true)
	testResolveEqualsParity(t, "provider and equaler",
		equalsProviderAndEqualer{"x", "1"}, equalsProviderAndEqualer{"x", "2"}, true)
}

func TestResolveEqualsProviderNil(t *testing.T) {
	// A nil EqualsFunc result is a programmer error, reported at resolution time.
	assertPanics(t, func() { resolveEquals[equalsProviderNil]() })
}

func TestResolveEqualsEmbeddedProto(t *testing.T) {
	a := equalsEmbedsProto{WorkloadSelector: &v1beta1.WorkloadSelector{}, Extra: "x"}
	b := equalsEmbedsProto{WorkloadSelector: &v1beta1.WorkloadSelector{}, Extra: "x"}
	// Embedded protos cannot be compared; both the probing and resolved paths panic at
	// comparison time.
	assertPanics(t, func() { Equal(a, b) })
	eq := resolveEquals[equalsEmbedsProto]()
	assertPanics(t, func() { eq(a, b) })
}

func TestEqualsForCollection(t *testing.T) {
	// An explicit WithEquals function is used as-is.
	fn := equalsForCollection[equalsValueValue](collectionOptions{
		name:   "explicit",
		equals: func(a, b equalsValueValue) bool { return a.B == b.B },
	})
	assert.Equal(t, fn(equalsValueValue{"x", "1"}, equalsValueValue{"y", "1"}), true)

	// A WithEquals function for the wrong type panics at construction.
	assertPanics(t, func() {
		equalsForCollection[equalsValueValue](collectionOptions{
			name:   "mismatch",
			equals: func(a, b int) bool { return a == b },
		})
	})

	// No explicit function falls back to resolution.
	fn = equalsForCollection[equalsValueValue](collectionOptions{name: "resolved"})
	assert.Equal(t, fn(equalsValueValue{"x", "1"}, equalsValueValue{"x", "2"}), true)
}

// The key* types below cover the receiver shapes GetKey and resolveKey probe for.

type keyNamer struct{ A, B string }

func (k keyNamer) ResourceName() string { return k.A }

type keyNamerIface interface{ ResourceName() string }

type keyProviderOnly struct{ A, B string }

func (keyProviderOnly) ResourceNameFunc() func(keyProviderOnly) string {
	return func(k keyProviderOnly) string { return k.A }
}

type keyProviderPointerReceiver struct{ A, B string }

func (*keyProviderPointerReceiver) ResourceNameFunc() func(keyProviderPointerReceiver) string {
	return func(k keyProviderPointerReceiver) string { return k.A }
}

type keyProviderAndNamer struct{ A, B string }

func (k keyProviderAndNamer) ResourceName() string { return k.A }

func (keyProviderAndNamer) ResourceNameFunc() func(keyProviderAndNamer) string {
	return keyProviderAndNamer.ResourceName
}

type keyProviderNil struct{ A string }

func (keyProviderNil) ResourceNameFunc() func(keyProviderNil) string { return nil }

type keyless struct{ A string }

func testResolveKeyParity[O any](t *testing.T, name string, o O, want string) {
	t.Helper()
	assert.Equal(t, GetKey(o), want, name+": GetKey")
	assert.Equal(t, string(resolveKey[O]()(o)), want, name+": resolveKey")
}

func TestResolveKey(t *testing.T) {
	testResolveKeyParity(t, "string", "ns/name", "ns/name")
	testResolveKeyParity(t, "object",
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "ns"}}, "ns/name")
	testResolveKeyParity(t, "config",
		config.Config{Meta: config.Meta{Name: "name", Namespace: "ns"}}, keyFunc("name", "ns"))
	testResolveKeyParity(t, "config pointer",
		&config.Config{Meta: config.Meta{Name: "name", Namespace: "ns"}}, keyFunc("name", "ns"))
	testResolveKeyParity(t, "namer", keyNamer{"x", "1"}, "x")
	testResolveKeyParity(t, "provider", keyProviderOnly{"x", "1"}, "x")
	testResolveKeyParity(t, "provider pointer receiver", keyProviderPointerReceiver{"x", "1"}, "x")
	testResolveKeyParity(t, "provider and namer", keyProviderAndNamer{"x", "1"}, "x")
	// Interface elements resolve per call based on the dynamic type.
	testResolveKeyParity[keyNamerIface](t, "interface", keyNamer{"x", "1"}, "x")

	// A keyless type panics at key computation time in both paths.
	assertPanics(t, func() { GetKey(keyless{"x"}) })
	assertPanics(t, func() { resolveKey[keyless]()(keyless{"x"}) })
	// A nil ResourceNameFunc result is a programmer error, reported at resolution time.
	assertPanics(t, func() { resolveKey[keyProviderNil]() })
}

func assertPanics(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	f()
}

// benchEqualsBig approximates a fat output element like model.ServiceInfo.
type benchEqualsBig struct {
	S1, S2, S3 string
	M          map[string]string
	P          *int
	Pad        [320]byte
}

func (b benchEqualsBig) Equals(o benchEqualsBig) bool { return b.S1 == o.S1 && b.S2 == o.S2 }

func (b benchEqualsBig) ResourceName() string { return b.S1 }

// benchEqualsBigProvider is benchEqualsBig with an EqualerProvider implementation, kept separate
// so the probe/resolved cases above still measure the Equaler tiers.
type benchEqualsBigProvider struct {
	S1, S2, S3 string
	M          map[string]string
	P          *int
	Pad        [320]byte
}

func (b benchEqualsBigProvider) Equals(o benchEqualsBigProvider) bool {
	return b.S1 == o.S1 && b.S2 == o.S2
}

func (benchEqualsBigProvider) EqualsFunc() func(a, b benchEqualsBigProvider) bool {
	return benchEqualsBigProvider.Equals
}

func (b benchEqualsBigProvider) ResourceName() string { return b.S1 }

func (benchEqualsBigProvider) ResourceNameFunc() func(benchEqualsBigProvider) string {
	return benchEqualsBigProvider.ResourceName
}

func BenchmarkEquals(b *testing.B) {
	x := benchEqualsBig{S1: "a", S2: "b", S3: "c", M: map[string]string{"k": "v"}}
	y := benchEqualsBig{S1: "a", S2: "b", S3: "d", M: map[string]string{"k": "v"}}
	b.Run("probe", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			if !Equal(x, y) {
				b.Fatal("expected equal")
			}
		}
	})
	b.Run("resolved", func(b *testing.B) {
		eq := resolveEquals[benchEqualsBig]()
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if !eq(x, y) {
				b.Fatal("expected equal")
			}
		}
	})
	b.Run("with-equals", func(b *testing.B) {
		eq := equalsForCollection[benchEqualsBig](collectionOptions{equals: benchEqualsBig.Equals})
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if !eq(x, y) {
				b.Fatal("expected equal")
			}
		}
	})
	b.Run("provider", func(b *testing.B) {
		px := benchEqualsBigProvider{S1: "a", S2: "b", S3: "c", M: map[string]string{"k": "v"}}
		py := benchEqualsBigProvider{S1: "a", S2: "b", S3: "d", M: map[string]string{"k": "v"}}
		eq := resolveEquals[benchEqualsBigProvider]()
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if !eq(px, py) {
				b.Fatal("expected equal")
			}
		}
	})
}

func BenchmarkGetKey(b *testing.B) {
	x := benchEqualsBig{S1: "ns/name", S2: "b", M: map[string]string{"k": "v"}}
	b.Run("probe", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			if GetKey(x) == "" {
				b.Fatal("expected key")
			}
		}
	})
	b.Run("resolved", func(b *testing.B) {
		key := resolveKey[benchEqualsBig]()
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if key(x) == "" {
				b.Fatal("expected key")
			}
		}
	})
	b.Run("provider", func(b *testing.B) {
		px := benchEqualsBigProvider{S1: "ns/name", S2: "b", M: map[string]string{"k": "v"}}
		key := resolveKey[benchEqualsBigProvider]()
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if key(px) == "" {
				b.Fatal("expected key")
			}
		}
	})
}
