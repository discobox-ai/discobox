// The sandbox's system layer owns folder trust, so a later direct `codex`
// start sees it too. User preferences and portable capture never carry paths.
const fs = require('node:fs');
const { parse } = require('/usr/local/libexec/discobox/harness-capture/settings.cjs');

const configPath = '/etc/codex/config.toml';
const baseline = fs.readFileSync('/usr/local/libexec/discobox/codex-system-config.toml', 'utf8');
const previous = fs.existsSync(configPath) ? parse(fs.readFileSync(configPath, 'utf8'), 'toml') : {};
const paths = new Set(Object.keys(previous.projects || {}));
paths.add(fs.realpathSync(process.argv[2]));
const projects = [...paths].sort().map(path => `${JSON.stringify(path)}={trust_level="trusted"}`).join(',');
// Write atomically while the caller holds the system-layer lock. Put projects
// before any table headers, so it remains a top-level inline TOML value.
const content = `projects={${projects}}\n${baseline}`;
if (!fs.existsSync(configPath) || fs.readFileSync(configPath, 'utf8') !== content) {
  const temporary = configPath + '.tmp';
  fs.writeFileSync(temporary, content, { mode: 0o644 });
  fs.renameSync(temporary, configPath);
}
