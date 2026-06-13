package apiclient

//go:generate sh -c "go run ../../cmd/openapi -output /tmp/discobox-openapi-client.json -downgrade -omit-unsupported-client-operations && go tool ogen --config ogen.yml --target gen --clean --package apiclientgen /tmp/discobox-openapi-client.json && rm -f /tmp/discobox-openapi-client.json"
