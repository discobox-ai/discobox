package poolagent

//go:generate go -C .. tool ogen --config pool-agent/api/gen/ogen.yml --target pool-agent/api/gen --package poolagentapi --clean pool-agent/api/openapi/pool.yaml
//go:generate go run ./api/internal/genmodelaliases -openapi ./api/openapi/pool.yaml -schemas ./api/gen/oas_schemas_gen.go -out ./api/model/aliases_gen.go -gen-import github.com/obot-platform/discobox/pool-agent/api/gen -gen-package poolagentapi -package-doc "Package model exposes stable aliases for generated pool-agent API schema types."
