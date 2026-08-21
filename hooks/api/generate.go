package api

//go:generate go -C ../.. tool ogen --config hooks/api/gen/ogen.yml --target hooks/api/gen --clean --package hookapigen hooks/api/openapi/hooks.yaml
//go:generate go run ./internal/genmodelaliases -schema-dir ./openapi/components/model -out ./model/aliases_gen.go -gen-import github.com/discobox-ai/discobox/hooks/api/gen -gen-package hookapigen
