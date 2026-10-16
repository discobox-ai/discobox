package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/x/id"
)

// idPrefixProjectGateway is this package's own: nothing outside the server
// mints a project gateway.
const idPrefixProjectGateway = "gw"

// ProjectGateway is one gateway every discobox of a project is routed through
// (ADR 26-10-16-470): a request beneath Match goes to Target, with
// RequestHeaders applied, once every other policy has judged it as the
// discobox sent it. It is addressed by Name within its project; Match is
// unique there too, so a request matches at most one. Match and Target are
// stored as gateway.Normalize writes them.
type ProjectGateway struct {
	ID             string                 `gorm:"primaryKey;type:text"`
	ProjectID      string                 `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_project_gateway_name,priority:1;uniqueIndex:idx_project_gateway_match,priority:1"`
	Name           string                 `gorm:"column:name;not null;type:text;uniqueIndex:idx_project_gateway_name,priority:2"`
	Match          string                 `gorm:"column:match_url;not null;type:text;uniqueIndex:idx_project_gateway_match,priority:2"`
	Target         string                 `gorm:"column:target_url;not null;type:text"`
	RequestHeaders gateway.HeaderModifier `gorm:"column:request_headers;type:text;serializer:json"`
	CreatedBy      string                 `gorm:"column:created_by;not null;type:text;default:''"`
	CreatedAt      time.Time
	UpdatedAt      time.Time

	Project *Project `gorm:"foreignKey:ProjectID"`
}

func (ProjectGateway) TableName() string { return "project_gateways" }

func (g *ProjectGateway) BeforeCreate(_ *gorm.DB) error {
	if g.ID == "" {
		var err error
		if g.ID, err = id.New(idPrefixProjectGateway); err != nil {
			return err
		}
	}
	return nil
}

// Route is the gateway as the proxy applies it.
func (g ProjectGateway) Route() gateway.Route {
	return gateway.Route{Name: g.Name, Match: g.Match, Target: g.Target, RequestHeaders: g.RequestHeaders}
}
