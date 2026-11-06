package pooloutbound

import (
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"
)

const Root = "/etc/discobox/pool-outbound"
const RuntimeFile = Root + "/current/runtime.json"
const EnvironmentFile = Root + "/current/environment"
const InstallFile = "/etc/discobox/install-pool-outbound"
const RevisionEnv = "DISCOBOX_POOL_OUTBOUND_REVISION"

func StagedRuntimeFile(revision string) string {
	return path.Join(Root, "revisions", revision, "runtime.json")
}

// Files stages one immutable revision before provisioning. The current symlink
// is published only after trust installation succeeds, so services never read a
// mixture of two environment/configuration documents.
func (r Resolved) Files() (map[string][]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	stage := path.Dir(StagedRuntimeFile(r.Revision()))
	files := map[string][]byte{stage + "/runtime.json": raw, InstallFile: []byte(InstallScript)}
	anchors, err := Anchors(r.CAPEM)
	if err != nil {
		return nil, err
	}
	var fingerprints []string
	for fp, cert := range anchors {
		files[stage+"/anchors/"+fp+".crt"] = []byte(cert)
		fingerprints = append(fingerprints, fp)
	}
	sort.Strings(fingerprints)
	files[stage+"/anchors.list"] = []byte(strings.Join(fingerprints, "\n") + "\n")
	var env strings.Builder
	for _, entry := range r.Environment()[1:] {
		key, value, _ := strings.Cut(entry, "=")
		env.WriteString(key)
		env.WriteByte('=')
		env.WriteString(strconv.Quote(value))
		env.WriteByte('\n')
	}
	files[stage+"/environment"] = []byte(env.String())
	files[stage+"/apt.conf"] = []byte(r.APTConfig())
	return files, nil
}

// InstallScript regenerates the destination trust bundle atomically. A failed
// regeneration restores the previous managed anchors and leaves current alone.
const InstallScript = `set -eu
revision=${DISCOBOX_POOL_OUTBOUND_REVISION:?missing pool outbound revision}
case "$revision" in *[!a-f0-9]*|'') exit 1;; esac
[ "${#revision}" -eq 64 ]
root=/etc/discobox/pool-outbound
stage=$root/revisions/$revision
anchors=/usr/local/share/ca-certificates/discobox-outbound
install -d -m 755 "$anchors"
backup=$(mktemp -d)
cp -a "$anchors/." "$backup/"
rollback() {
 rm -f "$anchors/"*.crt
 cp -a "$backup/." "$anchors/"
 update-ca-certificates >/dev/null 2>&1 || true
 rm -rf "$backup"
}
trap rollback EXIT
for cert in "$anchors/"*.crt; do
 [ -e "$cert" ] || continue
 fp=$(basename "$cert" .crt)
 if ! grep -qxF "$fp" "$stage/anchors.list"; then rm -f "$cert"; fi
done
while IFS= read -r fp; do
 [ -n "$fp" ] || continue
 install -m 644 "$stage/anchors/$fp.crt" "$anchors/$fp.crt"
done < "$stage/anchors.list"
update-ca-certificates >/dev/null
install -d -m 755 /etc/systemd/system/docker.service.d
printf '%s\n' '[Service]' 'EnvironmentFile=/etc/discobox/pool-outbound/current/environment' > /etc/systemd/system/docker.service.d/20-discobox-outbound.conf
install -d -m 755 /etc/apt/apt.conf.d
ln -sfn /etc/discobox/pool-outbound/current/apt.conf /etc/apt/apt.conf.d/90-discobox-outbound
systemctl daemon-reload
ln -sfn "revisions/$revision" "$root/current.next"
mv -Tf "$root/current.next" "$root/current"
trap - EXIT
rm -rf "$backup"
`

// APTConfig makes empty schemes authoritative for apt as well: DIRECT stops
// apt's HTTPS transport from falling back to its HTTP proxy configuration.
func (r Resolved) APTConfig() string {
	endpoint := func(raw string) string {
		if raw == "" {
			return "DIRECT"
		}
		return raw
	}
	return "Acquire::http::Proxy " + strconv.Quote(endpoint(r.HTTP)) + ";\nAcquire::https::Proxy " + strconv.Quote(endpoint(r.HTTPS)) + ";\n"
}
