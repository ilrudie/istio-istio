package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"istio.io/istio/pkg/kube/krt"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("You must specify the JSON file name as the first argument.")
	}

	fileName := os.Args[1]
	file, err := os.ReadFile(fileName)
	if err != nil {
		log.Fatalf("Error reading file: %v", err)
	}

	var debugData krt.DumpedState
	err = json.Unmarshal(file, &debugData)
	if err != nil {
		log.Fatalf("Error unmarshalling JSON: %v", err)
	}

	fmt.Printf("%s\n", krt.FormatMermaid(debugData))
}
