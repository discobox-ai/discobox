package service

import "errors"

// ErrNotFound reports that the requested hook or resource does not exist.
var ErrNotFound = errors.New("not found")
