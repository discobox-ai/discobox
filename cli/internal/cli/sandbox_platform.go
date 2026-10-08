package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandboxpath"
)

// undeclaredPlatform is what a discobox or a pool that has declared no platform
// is taken to run, for judging paths in it and for nothing else: Linux. A
// platform is undeclared on a discobox or pool from before platforms were
// recorded, every one of which is a Linux container, and on a pool whose agent
// has not reported yet. Only the OS is read; the architecture spells no path
// differently.
var undeclaredPlatform = platform.Platform{OS: "linux", Arch: runtime.GOARCH}

// declaredPlatform reads a platform as the API spells it, taking an undeclared
// or unreadable one as undeclaredPlatform.
func declaredPlatform(spelled string) platform.Platform {
	p, err := platform.Parse(spelled)
	if err != nil || p.IsZero() {
		return undeclaredPlatform
	}
	return p
}

// sandboxPaths is the path rules of an existing discobox: its own platform's
// (ADR 0145 §6).
func sandboxPaths(sandbox *apimodel.Sandbox) sandboxpath.Paths {
	return sandboxpath.For(declaredPlatform(sandbox.Platform.Or("")))
}

// newSandboxPlacement is the pool a new discobox goes on and the platform that
// pool hosts, which every path the create places in the discobox is judged by.
//
// pool is what --pool named — an ID, a short ID, or a name — and is resolved to
// the pool's ID, which the create then asks for. Empty leaves placement to the
// server, which puts a discobox on the project's default pool, so that is the
// pool whose platform is read; the ID stays empty, because the server's answer
// is the one that counts. A project with no default pool has no platform to
// read, and its create fails on the server for that reason rather than here.
//
// A caller the server refuses these reads (403) is a discobox creating
// another, whose role reads no project and no pool (ADR 0140 §4). With no
// --pool it creates on the default pool with undeclaredPlatform, as every
// create did before a platform was read, and as the SSH sync after it goes
// ahead without the sync. A pool it named is refused instead: that pool is a
// choice of platform, and paths placed for a guessed one would name places the
// discobox does not have.
func (a *App) newSandboxPlacement(ctx context.Context, client *apiclientgen.Client, projectID, pool string) (string, platform.Platform, error) {
	pool = strings.TrimSpace(pool)
	poolID, p, err := a.readNewSandboxPlacement(ctx, client, projectID, pool)
	var refused *plainStatusError
	if errors.As(err, &refused) && refused.code == http.StatusForbidden {
		if pool != "" {
			return "", platform.Platform{}, fmt.Errorf("--pool %s: the server refuses this caller a read of the pool, so what platform the discobox would run on is unknown; leave --pool out to create on the project's default pool", pool)
		}
		return "", undeclaredPlatform, nil
	}
	return poolID, p, err
}

func (a *App) readNewSandboxPlacement(ctx context.Context, client *apiclientgen.Client, projectID, pool string) (string, platform.Platform, error) {
	var poolID, lookup string
	if pool != "" {
		resolved, err := a.resolvePoolID(ctx, client, projectID, pool)
		if err != nil {
			return "", platform.Platform{}, err
		}
		poolID, lookup = resolved, resolved
	} else {
		res, err := client.GetProject(ctx, apiclientgen.GetProjectParams{ProjectId: projectID})
		if err != nil {
			return "", platform.Platform{}, err
		}
		project, err := expectResponse[apimodel.Project](res)
		if err != nil {
			return "", platform.Platform{}, err
		}
		lookup = project.DefaultPoolId.Or("")
		if lookup == "" {
			return "", undeclaredPlatform, nil
		}
	}
	res, err := client.GetPool(ctx, apiclientgen.GetPoolParams{ProjectId: projectID, PoolId: lookup})
	if err != nil {
		return "", platform.Platform{}, err
	}
	found, err := expectResponse[apimodel.Pool](res)
	if err != nil {
		return "", platform.Platform{}, err
	}
	return poolID, declaredPlatform(found.Platform.Or("")), nil
}
