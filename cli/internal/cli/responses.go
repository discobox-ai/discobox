package cli

import (
	"fmt"
	"net/http"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

func expectResponse[T any](res any) (*T, error) {
	if out, ok := res.(*T); ok {
		return out, nil
	}
	return nil, responseError(res)
}

func expectNoContent[T any](res any) error {
	_, err := expectResponse[T](res)
	return err
}

func responseError(res any) error {
	if problem, ok := res.(*apiclientgen.ErrorModelStatusCode); ok {
		status := problem.StatusCode
		title := problem.Response.Title.Value
		detail := problem.Response.Detail.Value
		switch {
		case title != "" && detail != "":
			return fmt.Errorf("request failed: %d %s: %s", status, title, detail)
		case title != "":
			return fmt.Errorf("request failed: %d %s", status, title)
		case detail != "":
			return fmt.Errorf("request failed: %d %s", status, detail)
		case status != 0:
			return fmt.Errorf("request failed: %d %s", status, http.StatusText(status))
		}
	}
	if problem, ok := res.(*apiclientgen.ErrorResponseStatusCode); ok {
		status := problem.StatusCode
		message := problem.Response.Error
		if message != "" {
			return fmt.Errorf("request failed: %d %s", status, message)
		}
		if status != 0 {
			return fmt.Errorf("request failed: %d %s", status, http.StatusText(status))
		}
	}
	return fmt.Errorf("unexpected response type %T", res)
}
