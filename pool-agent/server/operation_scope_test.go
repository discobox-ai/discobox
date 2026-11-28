package server

import (
	"os"
	"regexp"
	"testing"
)

// Every operation of the generated pool API needs a scope: one missing from
// requiredPoolOperationScope is reachable with any token minted for the pool,
// a sandbox's proxy token included. The pool's refresh route shipped without
// one once; this walks the generated operation names, so the next route
// cannot.
func TestEveryPoolOperationRequiresAScope(t *testing.T) {
	data, err := os.ReadFile("../api/gen/oas_operations_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	operations := regexp.MustCompile(`(?m)^\s+\w+Operation\s+OperationName = "(\w+)"$`).FindAllStringSubmatch(string(data), -1)
	if len(operations) == 0 {
		t.Fatal("read no operation names from the generated API")
	}
	for _, operation := range operations {
		if requiredPoolOperationScope(operation[1]) == "" {
			t.Errorf("pool operation %s requires no scope", operation[1])
		}
	}
}
