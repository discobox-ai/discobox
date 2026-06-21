package model

import apigen "github.com/obot-platform/discobox/api/gen"

const (
	EventTypeResourceChanged = apigen.EventTypeResourceChanged
	EventTypeResourceListed  = apigen.EventTypeResourceListed

	EventActionCreated = apigen.EventActionCreated
	EventActionUpdated = apigen.EventActionUpdated
	EventActionDeleted = apigen.EventActionDeleted
	EventActionListed  = apigen.EventActionListed
)

type ProjectEvent = apigen.ProjectEvent
