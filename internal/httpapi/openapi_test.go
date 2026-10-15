package httpapi

import (
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const openAPIPath = "../../api/openapi.yaml"

var (
	pathLine   = regexp.MustCompile(`^  (/[^:\s]*):\s*$`)
	methodLine = regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
)

// documentedOperations lists "METHOD /path" for every operation in the OpenAPI file. A line scan is enough
// for the layout this file uses (paths indented two spaces, methods four) and avoids a YAML dependency.
func documentedOperations(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	var operations []string
	var current string
	inPaths := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && line != "" && !strings.HasPrefix(line, " "):
			inPaths = false
		case inPaths && pathLine.MatchString(line):
			current = pathLine.FindStringSubmatch(line)[1]
		case inPaths && methodLine.MatchString(line):
			operations = append(operations, strings.ToUpper(methodLine.FindStringSubmatch(line)[1])+" "+current)
		}
	}
	return operations
}

func TestOpenAPI_DocumentsExactlyTheRegisteredRoutes(t *testing.T) {
	a := &api{}
	noop := func(func(http.ResponseWriter, *http.Request) error, ...Middleware) http.Handler {
		return http.NotFoundHandler()
	}
	registered := a.routes(noop, func(next http.Handler) http.Handler { return next }).patterns
	documented := documentedOperations(t)

	for _, route := range registered {
		if !slices.Contains(documented, route) {
			t.Errorf("route %q is registered but missing from api/openapi.yaml", route)
		}
	}
	for _, operation := range documented {
		if !slices.Contains(registered, operation) {
			t.Errorf("operation %q is in api/openapi.yaml but no such route is registered", operation)
		}
	}
}

func TestOpenAPI_DocumentsEveryErrorCode(t *testing.T) {
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	for _, code := range []string{
		"VALIDATION-01", "VALIDATION-02", "VALIDATION-03", "VALIDATION-04", "VALIDATION-05", "VALIDATION-06",
		"VALIDATION-07", "VALIDATION-08", "VALIDATION-09", "VALIDATION-12", "VALIDATION-13",
		"RESOURCE-NOT-FOUND-00", "RESOURCE-NOT-FOUND-01", "RESOURCE-NOT-FOUND-02", "RESOURCE-NOT-FOUND-03",
		"CONFLICT-00", "CONFLICT-01", "CONFLICT-02", "CONFLICT-03", "CONFLICT-04",
		"AUTH-01", "AUTH-02", "RATE-00", "INTERNAL-00",
	} {
		if !strings.Contains(text, code) {
			t.Errorf("error code %s is not documented in api/openapi.yaml", code)
		}
	}
}
