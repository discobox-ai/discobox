package cli

import "errors"

var errMissingProject = errors.New("project ID is required; pass --project or set DISCOBOX_PROJECT")
