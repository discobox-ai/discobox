package services

import (
	"encoding/json"
)

// CurrentTerminalHarness composes the control-plane-owned selection with a
// terminal's posted observation, preserving its historical harnessId and fields
// unknown to this server. A plain exec has no harnessId and remains unchanged.
func CurrentTerminalHarness(record json.RawMessage, harnessID string) (json.RawMessage, error) {
	if harnessID == "" {
		return record, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(record, &fields); err != nil {
		return nil, err
	}
	raw, ok := fields["harnessId"]
	if !ok || string(raw) == "null" {
		return record, nil
	}
	var historical string
	if err := json.Unmarshal(raw, &historical); err != nil {
		return nil, err
	}
	if historical == "" {
		return record, nil
	}
	encoded, err := json.Marshal(harnessID)
	if err != nil {
		return nil, err
	}
	fields["currentHarnessId"] = encoded

	return json.Marshal(fields)
}

func sandboxAgentStatusCurrentHarness(status []byte, harnessID string) ([]byte, error) {
	if harnessID == "" {
		return status, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(status, &fields); err != nil {
		return nil, err
	}
	raw, ok := fields["sessions"]
	if !ok {
		return status, nil
	}
	var sessions []json.RawMessage
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, err
	}
	for i, session := range sessions {
		selected, err := CurrentTerminalHarness(session, harnessID)
		if err != nil {
			return nil, err
		}
		sessions[i] = selected
	}
	var err error
	fields["sessions"], err = json.Marshal(sessions)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}
