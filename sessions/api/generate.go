package api

//go:generate go -C ../.. tool ogen --config sessions/api/gen/ogen.yml --target sessions/api/gen --clean --package sessionapigen sessions/api/openapi/sessions.yaml
//go:generate go -C ../.. tool ogen --config sessions/api/supervisorgen/ogen.yml --target sessions/api/supervisorgen --clean --package supervisorapigen sessions/api/openapi/supervisor.yaml
