# Harness Capture Review Notes

- Check a legacy recognizer (`UpgradeLegacyJSON`'s seed and suffix lists)
  against every template any supported release left in a database, not the
  newest one: what each tag's `harness/*/image.json` declared, and what its
  configure scripts and data migrations wrote into `configured_files` and
  `save_back` (Codex trust stanzas, `{{ .secrets.X }}` fields). That is why
  `legacyCodexTrust` holds suffixes no image ever shipped; never drop one.
  One unrecognized template fails the whole migration (#553: only the v0.19
  rc Claude Code seed was listed, so every v0.18 install refused to start).
  `testdata/shipped-templates.json` holds the images' templates, from the tags.
