package poolagent

//go:generate go -C .. run ./api/internal/genogen -config pool-agent/api/gen/ogen.yml -target pool-agent/api/gen -package poolagentapi pool-agent/api/openapi/pool.yaml
//go:generate go run ./api/internal/genmodelaliases -openapi ./api/openapi/pool.yaml -schemas ./api/gen/oas_schemas_gen.go -out ./api/model/aliases_gen.go -gen-import github.com/discobox-ai/discobox/pool-agent/api/gen -gen-package poolagentapi -package-doc "Package model exposes stable aliases for generated pool-agent API schema types."
