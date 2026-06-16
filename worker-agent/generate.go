package workeragent

//go:generate go tool ogen --config api/clientgen/ogen.yml --target api/clientgen --package workeragentclientgen --clean api/openapi/sandbox.json
//go:generate go tool ogen --config api/servergen/ogen.yml --target api/servergen --package workeragentservergen --clean api/openapi/sandbox.json
