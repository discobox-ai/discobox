package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// primaryExecID is the id a client reads a sandbox's primary terminal by
// without knowing its exec id. The sandbox agent resolves it against its live
// terminals; a stored read resolves it against the record marked primary.
const primaryExecID = "primary"

// AcquireRunningSandboxHTTPClient leases a client for a read the sandbox's
// inventory can answer (ADR 26-10-11-657 §4).
//
// Its use is an inspect or an observe, which the lease marks on its tokens.
// Whether the sandbox is running is the pool agent's to say, not the row's:
// it knows the container's true state, refuses a read so marked of a sandbox
// that is not running instead of starting it, and the caller answers that
// refusal from the inventory. The row lags — an action that has just started a
// stopped sandbox is followed by reads of it before the pool's state report
// lands — and a read answered from the inventory then would miss the very
// exec the action created.
//
// The row decides only when the pool cannot be asked. A sandbox recorded as
// stopped on a pool that is not reachable is refused with
// sandbox.ErrNotRunning, so it is still read from its inventory.
func (s *Service) AcquireRunningSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	lease, sandboxModel, err := s.AcquireSandboxHTTPClient(ctx, projectID, sandboxID, scopes, use)
	if err != nil && errors.Is(err, sandbox.ErrPoolNotReachable) && sandboxModel != nil &&
		(sandboxModel.RuntimeState == model.SandboxRuntimeStateStopped || sandboxModel.RuntimeState == model.SandboxRuntimeStateSuspended) {
		return nil, sandboxModel, apperrors.StatusError{
			Status:  http.StatusConflict,
			Message: fmt.Sprintf("discobox %s is %s, and its pool is not reachable", sandboxModel.ID, sandboxModel.RuntimeState),
			Cause:   sandbox.ErrNotRunning,
		}
	}
	return lease, sandboxModel, err
}

// ReadSandboxInventory answers one read from the inventory the sandbox last
// posted (ADR 26-10-11-657 §4).
//
// The answer is the part as the sandbox agent posted it, not re-encoded from a
// model of it, so a field this server does not know still reaches the client,
// with observedAt, when the sandbox last had this inventory. After a harness
// switch, currentHarnessId alone is derived from its selected config; the
// historical harnessId and other observations remain as posted.
// A live answer has none, which is how a client tells them apart. Records are
// served as posted — an exec last seen running reads as running — because
// what the sandbox last said is the whole of what is known.
func (s *Service) ReadSandboxInventory(ctx context.Context, projectID, sandboxID string, scopes []string, read services.InventoryRead) ([]byte, error) {
	if err := authorizeRequestedScopes(ctx, scopes); err != nil {
		return nil, err
	}
	sb, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	inventory, err := s.store.GetSandboxInventory(ctx, projectID, sandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"discobox %s is not running and has posted nothing to read. Reading it does not start it: start it with `discobox start %s`, and what it then posts is readable whether it is running or not", sandboxID, sandboxID))
	}
	if err != nil {
		return nil, err
	}
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(inventory.Document, &parts); err != nil {
		return nil, fmt.Errorf("decode sandbox inventory: %w", err)
	}
	part := func(name string) (json.RawMessage, error) {
		raw, ok := parts[name]
		if !ok {
			return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
				"discobox %s is not running, and what it last posted has no %s. Reading it does not start it: start it with `discobox start %s` to read them", sandboxID, name, sandboxID))
		}
		return raw, nil
	}
	// Only the new selection field is derived. The inventory and every other
	// posted observation remain as recorded, with their original observedAt.
	if sb.HarnessChangeID != "" && sb.HarnessConfigID != nil {
		if raw, ok := parts["execs"]; ok {
			var records []json.RawMessage
			if err := json.Unmarshal(raw, &records); err != nil {
				return nil, err
			}
			for i, record := range records {
				selected, err := services.CurrentTerminalHarness(record, *sb.HarnessConfigID)
				if err != nil {
					return nil, err
				}
				records[i] = selected
			}
			parts["execs"], err = json.Marshal(records)
			if err != nil {
				return nil, err
			}
		}
	}
	observedAt := inventory.ObservedAt.UTC()
	switch read.Part {
	case services.InventoryExecs, services.InventoryServices, services.InventoryTools:
		raw, err := part(string(read.Part))
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{string(read.Part): raw, "observedAt": observedAt})
	case services.InventoryExec:
		raw, err := part("execs")
		if err != nil {
			return nil, err
		}
		record, _, err := inventoryExec(raw, read.ID)
		if err != nil {
			return nil, err
		}
		return withObservedAt(record, observedAt)
	case services.InventoryService:
		raw, err := part("services")
		if err != nil {
			return nil, err
		}
		record, err := inventoryRecord(raw, read.ID)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox service not found")
		}
		return withObservedAt(record, observedAt)
	case services.InventoryScreen:
		raw, err := part("execs")
		if err != nil {
			return nil, err
		}
		_, exec, err := inventoryExec(raw, read.ID)
		if err != nil {
			return nil, err
		}
		raw, err = part("screens")
		if err != nil {
			return nil, err
		}
		var screens map[string]json.RawMessage
		if err := json.Unmarshal(raw, &screens); err != nil {
			return nil, fmt.Errorf("decode sandbox inventory screens: %w", err)
		}
		screen, ok := screens[exec.ID]
		if !ok {
			return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
				"discobox %s is not running, and it last posted no screen for exec %s: it has no terminal, or its terminal had ended", sandboxID, exec.ID))
		}
		return withObservedAt(screen, observedAt)
	default:
		return nil, fmt.Errorf("sandbox inventory answers no %q read", read.Part)
	}
}

// inventoryRecordKey is what a stored read finds a record by. Nothing else of
// a record is modeled here.
type inventoryRecordKey struct {
	ID        string    `json:"id"`
	Primary   bool      `json:"primary"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// inventoryRecords decodes a posted list into its records, as posted, each
// beside the key it is found by.
func inventoryRecords(list json.RawMessage) ([]json.RawMessage, []inventoryRecordKey, error) {
	var records []json.RawMessage
	if err := json.Unmarshal(list, &records); err != nil {
		return nil, nil, fmt.Errorf("decode sandbox inventory: %w", err)
	}
	keys := make([]inventoryRecordKey, len(records))
	for i, record := range records {
		if err := json.Unmarshal(record, &keys[i]); err != nil {
			return nil, nil, fmt.Errorf("decode sandbox inventory: %w", err)
		}
	}
	return records, keys, nil
}

// inventoryRecord returns the record of a posted list with this id, as
// posted, or nil when it holds none.
func inventoryRecord(list json.RawMessage, id string) (json.RawMessage, error) {
	records, keys, err := inventoryRecords(list)
	if err != nil {
		return nil, err
	}
	for i, key := range keys {
		if key.ID == id {
			return records[i], nil
		}
	}
	return nil, nil
}

// inventoryExec finds the exec a read names in the posted exec list: by its
// id, or, for the primary terminal's alias, the primary the sandbox agent
// itself would have answered with — a starting or running one if the list has
// one, else the most recently created (terminal.selectLivePrimary). A sandbox
// keeps the record of a primary that ended beside the one that replaced it,
// so the first record marked primary is not the answer.
func inventoryExec(execs json.RawMessage, id string) (json.RawMessage, inventoryRecordKey, error) {
	records, keys, err := inventoryRecords(execs)
	if err != nil {
		return nil, inventoryRecordKey{}, err
	}
	found := -1
	for i, key := range keys {
		if id != primaryExecID {
			if key.ID == id {
				found = i
				break
			}
			continue
		}
		if !key.Primary {
			continue
		}
		// installing is how a starting terminal reads while its harness is
		// set up.
		if key.Status == "installing" || key.Status == "starting" || key.Status == "running" {
			found = i
			break
		}
		if found < 0 || key.CreatedAt.After(keys[found].CreatedAt) {
			found = i
		}
	}
	if found < 0 {
		return nil, inventoryRecordKey{}, apperrors.NewStatusError(http.StatusNotFound, "sandbox exec not found")
	}
	return records[found], keys[found], nil
}

// withObservedAt adds observedAt to a posted JSON object, leaving every other
// field as posted.
func withObservedAt(object json.RawMessage, observedAt time.Time) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object, &fields); err != nil {
		return nil, fmt.Errorf("decode sandbox inventory: %w", err)
	}
	stamp, err := json.Marshal(observedAt)
	if err != nil {
		return nil, err
	}
	fields["observedAt"] = stamp
	return json.Marshal(fields)
}
