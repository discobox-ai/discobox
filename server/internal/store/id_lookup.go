package store

import (
	"reflect"
	"strings"

	"gorm.io/gorm"

	idpkg "github.com/discobox-ai/discobox/id"
)

func firstByID[T any](db *gorm.DB, column, value string) (*T, error) {
	value = strings.TrimSpace(value)
	var matches []T
	var query *gorm.DB
	if !idpkg.IsGenerated(value) {
		query = db.Where(column+" = ? OR "+column+" LIKE ?", value, value+"%")
	} else {
		query = db.Where(column+" = ?", value)
	}
	if err := query.Limit(3).Find(&matches).Error; err != nil {
		return nil, err
	}
	for i := range matches {
		if resourceID(matches[i]) == value {
			return &matches[i], nil
		}
	}
	if len(matches) != 1 {
		return nil, ErrNotFound
	}
	return &matches[0], nil
}

func resourceID(value any) string {
	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	field := v.FieldByName("ID")
	if !field.IsValid() || field.Kind() != reflect.String {
		return ""
	}
	return field.String()
}
