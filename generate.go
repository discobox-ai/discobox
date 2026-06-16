package discobox

//go:generate go tool ogen --config api/servergen/ogen.yml --target api/servergen --package serverapi --clean api/openapi/server.yaml
//go:generate go tool ogen --config api/clientgen/ogen.yml --target api/clientgen --package apiclientgen --clean api/openapi/server.yaml
