package jobqueue

import "github.com/google/uuid"

func NewID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func NewIDString() string {
	id, err := NewID()
	if err != nil {
		panic(err)
	}
	return id
}
