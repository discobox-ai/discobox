package handlers

import (
	"context"
	"errors"
	"net/http"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/go-faster/jx"
	"github.com/ogen-go/ogen/ogenerrors"
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
	opts = append(opts, serverapi.WithErrorHandler(problemErrorHandler))
	return serverapi.NewServer(New(services), opts...)
}

func apiError(err error) *serverapi.ErrorModelStatusCode {
	if err == nil {
		return nil
	}
	status := statusCodeForError(err)
	return &serverapi.ErrorModelStatusCode{
		StatusCode: status,
		Response:   errorModel(status, err),
	}
}

func problemErrorHandler(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	status := statusCodeForError(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)

	e := new(jx.Encoder)
	body := errorModel(status, err)
	body.Encode(e)
	_, _ = e.WriteTo(w)
}

func statusCodeForError(err error) int {
	status := ogenerrors.ErrorCode(err)
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode()
	}
	return status
}

func errorModel(status int, err error) apimodel.ErrorModel {
	return apimodel.ErrorModel{
		Status: serverapi.NewOptInt64(int64(status)),
		Title:  serverapi.NewOptString(http.StatusText(status)),
		Detail: serverapi.NewOptString(err.Error()),
	}
}
