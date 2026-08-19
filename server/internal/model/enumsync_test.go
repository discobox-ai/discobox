package model

// This test guards against enum drift between the server's Go code and the
// canonical API contract in api/openapi/server.yaml. Go has no first-class
// enumerable enum type, but every enum-valued model field carries a
// machine-readable `enum:"..."` struct tag; this test cross-checks those tags
// against the enums declared in server.yaml, in both directions.
//
// Every enum in server.yaml must be classified exactly one way:
//   - name-matched: a model struct with the same name declares the same json
//     property with an identical `enum:` tag;
//   - aliased: an API-only schema (create/update bodies, event views) that
//     mirrors a model enum, mapped in yamlEnumAliases;
//   - yaml-owned: no Go model counterpart, listed with a reason in
//     yamlOwnedEnums.
//
// Adding an enum value to only one side, or adding a new enum without
// classifying it, fails this test with instructions.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	rootopenapi "github.com/obot-platform/discobox/api/openapi"
	"gopkg.in/yaml.v3"
)

// yamlEnumAliases maps API-only schema properties to the model field that owns
// the value set. Both sides must stay identical.
var yamlEnumAliases = map[string]string{
	"CreateSecretBody.type":               "Secret.type",
	"CreateSecretRequestBody.type":        "SecretRequest.type",
	"CreateSecretGrantBody.scope":         "SecretGrant.scope",
	"ApproveSecretRequestBody.scope":      "SecretGrant.scope",
	"ResolveSandboxSecretResponse.status": "SecretRequest.status",
	// The sandbox owns the full existence vocabulary, so the tag on the embedded
	// ResourceLifecycle is authoritative for it. The pool no longer matches:
	// only a sandbox can be archived (ADR 0022 §1), so Pool.desiredState is a
	// narrowing and moved to yamlOwnedEnums below.
	"SandboxRuntime.desiredState": "ResourceLifecycle.desiredState",
	// The runtime axis is declared on Sandbox rather than on the embedded
	// ResourceLifecycle, because only a sandbox has one (ADR 0034). The API
	// carries it inside the runtime object, so the two names differ.
	"SandboxRuntime.runtimeState": "Sandbox.runtimeState",
}

// yamlOwnedEnums lists contract enums with no authoritative Go model tag, with
// the reason. The contract in server.yaml is the single source of truth for
// these; server code must follow it.
var yamlOwnedEnums = map[string]string{
	"Job.status": "job status values are owned by the orchestration module; model.Job.Status is untagged text",
	// Both axes now diverge per resource, which is what a single embedded tag
	// cannot express: the shared tag is the union, and each resource's schema
	// narrows it (ADR 0017 §2, ADR 0022 §1).
	"Pool.desiredState":                  "per-resource narrowing: a pool cannot be archived, so it omits the sandbox-only value in the shared tag",
	"Pool.state":                         "per-resource narrowing of the embedded ResourceLifecycle, whose shared tag is the union of both vocabularies",
	"SandboxRuntime.state":               "per-resource narrowing of the embedded ResourceLifecycle",
	"SandboxRuntime.displayState":        "derived presentation state computed by the API layer, not stored on the model",
	"PoolSandboxState.state":             "the pool agent's reporting vocabulary: the states a runtime can actually observe, a subset of the model's",
	"SandboxConfig.harnessMode":          "model.Sandbox.HarnessMode is untagged text; run/config is a contract-level restriction",
	"SandboxCreateConfig.harnessMode":    "model.Sandbox.HarnessMode is untagged text; run/config is a contract-level restriction",
	"SandboxExec.status":                 "exec lifecycle is owned by the sandbox-agent",
	"SandboxAgentSessionStatus.state":    "harness session state is computed and owned by sandbox-agent; the server stores AgentStatus as opaque JSON",
	"SandboxAgentListeningPort.protocol": "what a listening port speaks is established by sandbox-agent probing it (ADR 0046); the server stores AgentStatus as opaque JSON",
	"SandboxExecLogEntry.stream":         "exec log streams are owned by the sandbox-agent",
	"HarnessVolume.volume":               "value set is owned by harness.VolumeKind in the root module, not a server/internal/model enum tag",
	"SandboxUpgrade.reason":              "derived at read time by services.SandboxUpgrade from the pin and the harness config; nothing on the model stores it",
	"CreateProjectBody.copy[]":           "names the resource kinds a project copy takes; a request-shaping vocabulary with no persisted field behind it",
}

func TestModelEnumTagsMatchOpenAPI(t *testing.T) {
	modelEnums := parseModelEnumTags(t)
	yamlEnums := parseServerYAMLEnums(t)

	// Every alias must point from a live yaml enum to a live model enum, and
	// every yaml-owned entry must still exist, so the lists never go stale.
	for yamlKey, modelKey := range yamlEnumAliases {
		if _, ok := yamlEnums[yamlKey]; !ok {
			t.Errorf("yamlEnumAliases entry %q no longer exists in server.yaml; remove it", yamlKey)
		}
		if _, ok := modelEnums[modelKey]; !ok {
			t.Errorf("yamlEnumAliases target %q has no enum tag in the model package; fix the alias", modelKey)
		}
	}
	for yamlKey := range yamlOwnedEnums {
		if _, ok := yamlEnums[yamlKey]; !ok {
			t.Errorf("yamlOwnedEnums entry %q no longer exists in server.yaml; remove it", yamlKey)
		}
		if _, aliased := yamlEnumAliases[yamlKey]; aliased {
			t.Errorf("%q is both aliased and yaml-owned; pick one", yamlKey)
		}
	}

	// Every enum in server.yaml must be classified and, when it has a model
	// counterpart, identical to it.
	for yamlKey, yamlValues := range yamlEnums {
		modelKey := yamlKey
		if alias, ok := yamlEnumAliases[yamlKey]; ok {
			modelKey = alias
		}
		if modelValues, ok := modelEnums[modelKey]; ok {
			if !sameValueSet(yamlValues, modelValues) {
				t.Errorf("enum drift for %s: server.yaml has %v, model tag on %s has %v\n"+
					"update the `enum:\"...\"` tag and api/openapi/server.yaml together, then run `go tool task generate`",
					yamlKey, yamlValues, modelKey, modelValues)
			}
			continue
		}
		if _, ok := yamlOwnedEnums[yamlKey]; ok {
			continue
		}
		t.Errorf("unclassified enum %s in server.yaml: add a matching `enum:\"...\"` tag to the model field, "+
			"map it in yamlEnumAliases, or record it in yamlOwnedEnums with a reason", yamlKey)
	}

	// Reverse direction: a model enum tag whose schema and property exist in
	// server.yaml must be declared as an enum there too. A property that lost
	// its enum (or never gained one) is contract drift.
	yamlProps := parseServerYAMLProperties(t)
	for modelKey, modelValues := range modelEnums {
		if _, hasEnum := yamlEnums[modelKey]; hasEnum {
			continue
		}
		if yamlProps[modelKey] {
			t.Errorf("model field %s declares enum %v but server.yaml's matching property has no enum; "+
				"add it to api/openapi/server.yaml and run `go tool task generate`", modelKey, modelValues)
		}
	}
}

// parseModelEnumTags walks this package's source and returns
// "StructName.jsonFieldName" -> enum values for every field with an
// `enum:"..."` struct tag. Parsing source (rather than using reflection over a
// hand-kept type list) means new structs and fields are picked up
// automatically.
func parseModelEnumTags(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read model package dir: %v", err)
	}
	fset := token.NewFileSet()
	enums := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			structType, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range structType.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
				enumTag, ok := tag.Lookup("enum")
				if !ok || enumTag == "" {
					continue
				}
				jsonName, _, _ := strings.Cut(tag.Get("json"), ",")
				if jsonName == "" || jsonName == "-" {
					continue
				}
				enums[spec.Name.Name+"."+jsonName] = strings.Split(enumTag, ",")
			}
			return true
		})
	}
	if len(enums) == 0 {
		t.Fatal("no enum tags found in the model package; the parser is broken")
	}
	return enums
}

type yamlSchemaDoc struct {
	Components struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Enum  []string `yaml:"enum"`
				Items *struct {
					Enum []string `yaml:"enum"`
				} `yaml:"items"`
			} `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func parseServerYAMLDoc(t *testing.T) yamlSchemaDoc {
	t.Helper()
	var doc yamlSchemaDoc
	if err := yaml.Unmarshal(rootopenapi.ServerYAML, &doc); err != nil {
		t.Fatalf("parse server.yaml: %v", err)
	}
	if len(doc.Components.Schemas) == 0 {
		t.Fatal("no schemas found in server.yaml; the parser is broken")
	}
	return doc
}

// parseServerYAMLEnums returns "SchemaName.propertyName" -> enum values for
// every property (or array item, keyed "SchemaName.propertyName[]") declaring
// an enum in the canonical contract.
func parseServerYAMLEnums(t *testing.T) map[string][]string {
	t.Helper()
	doc := parseServerYAMLDoc(t)
	enums := map[string][]string{}
	for name, schema := range doc.Components.Schemas {
		for prop, spec := range schema.Properties {
			if len(spec.Enum) > 0 {
				enums[name+"."+prop] = spec.Enum
			}
			if spec.Items != nil && len(spec.Items.Enum) > 0 {
				enums[name+"."+prop+"[]"] = spec.Items.Enum
			}
		}
	}
	return enums
}

// parseServerYAMLProperties returns the set of "SchemaName.propertyName" keys
// present in the contract, enum or not, for the reverse drift check.
func parseServerYAMLProperties(t *testing.T) map[string]bool {
	t.Helper()
	doc := parseServerYAMLDoc(t)
	props := map[string]bool{}
	for name, schema := range doc.Components.Schemas {
		for prop := range schema.Properties {
			props[name+"."+prop] = true
		}
	}
	return props
}

func sameValueSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	return fmt.Sprint(as) == fmt.Sprint(bs)
}
