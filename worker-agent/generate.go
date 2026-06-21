package workeragent

//go:generate go -C .. tool ogen --config worker-agent/api/gen/ogen.yml --target worker-agent/api/gen --package workeragentapi --clean worker-agent/api/openapi/worker.yaml
//go:generate go run ./api/internal/genmodelaliases -openapi ./api/openapi/worker.yaml -schemas ./api/gen/oas_schemas_gen.go -out ./api/model/aliases_gen.go -gen-import github.com/obot-platform/discobox/worker-agent/api/gen -gen-package workeragentapi -package-doc "Package model exposes stable aliases for generated worker-agent API schema types."
