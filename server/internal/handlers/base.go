package handlers

import (
	"errors"
	"net/http"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

// Handler adapts server services to the generated OpenAPI server interface.
type Handler struct {
	services services.Services
}

var _ serverapi.Handler = (*Handler)(nil)

// New creates the generated API handler adapter.
func New(services services.Services) *Handler {
	return &Handler{services: services}
}

// NewServer creates an http.Handler from the generated OpenAPI server scaffold.
func NewServer(services services.Services, opts ...serverapi.ServerOption) (http.Handler, error) {
	return serverapi.NewServer(New(services), opts...)
}

func apiError(err error) *serverapi.ErrorModelStatusCode {
	if err == nil {
		return nil
	}
	status := http.StatusInternalServerError
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode()
	}
	return &serverapi.ErrorModelStatusCode{
		StatusCode: status,
		Response: apimodel.ErrorModel{
			Status: serverapi.NewOptInt64(int64(status)),
			Title:  serverapi.NewOptString(http.StatusText(status)),
			Detail: serverapi.NewOptString(err.Error()),
		},
	}
}
