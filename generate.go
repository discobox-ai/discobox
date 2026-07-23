package discobox

//go:generate go tool ogen --config api/gen/ogen.yml --target api/gen --package apigen --clean api/openapi/server.yaml
//go:generate go run ./api/internal/gensandboxopenapi
//go:generate go tool ogen --config api/gen/ogen.yml --target api/sandboxgen --package sandboxapigen --clean api/openapi/sandbox.yaml
//go:generate go run ./api/internal/genmodelaliases -openapi ./api/openapi/server.yaml -schemas ./api/gen/oas_schemas_gen.go -out ./api/model/aliases_gen.go -gen-import github.com/obot-platform/discobox/api/gen -gen-package apigen -package-doc "Package model exposes stable aliases for generated Server REST API schema types."
