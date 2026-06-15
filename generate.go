package discobox

//go:generate go run ./cmd/openapi -api public -output openapi.json -downgrade
//go:generate go run ./cmd/openapi -api worker -output worker-openapi.json -downgrade
