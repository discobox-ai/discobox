package console

import (
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/cli/internal/secretenv"
	"github.com/discobox-ai/discobox/pkg/secretformat"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// The secret forms: a credential stored or edited on one card, and a binding
// of one made by hand.

// A lifetime is a picker of presets, its keys a lifetime in seconds, and a
// typed field behind "custom": an hour, a day, a week, a month, forever, or
// one typed in. Seconds alone was a field nobody could answer without
// arithmetic, and 604800 is not a week to anyone reading it back. The presets
// and their spelling are the lifetime package's, which is also what the
// `discobox secret` flags parse.
const (
	// LifetimeNever is the preset that is no limit, or never expiring.
	LifetimeNever = "0"
	// LifetimeCustom is the preset that is the typed field below it.
	LifetimeCustom = "custom"
)

// SecretRouteKey prefixes the keys of a secret form's route fields, one per
// route and one empty for another.
const SecretRouteKey = "route:"

// ExchangeFieldPrefix prefixes the key of a field an exchange credential's
// field is typed into.
const ExchangeFieldPrefix = "exchange:"

// keepStored is what an untouched value field says on an existing credential:
// a value nobody can read back, and one nothing has to be typed to keep.
const keepStored = "unchanged — type to replace it"

// defaultValueLifetime is what a value got from a command lasts when nobody
// chooses otherwise, as the server gives one (model.DefaultRefreshTTL).
const defaultValueLifetime = 5 * time.Minute

// valueLifetimes are the lifetimes a token's value is offered: a value got
// from a command is short-lived by assumption, so these are minutes and hours,
// not the days and months a binding is given (ADR 26-09-25-122 §1).
var valueLifetimes = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 8 * time.Hour, lifetime.Day, lifetime.Forever}

// lifetimeFields is a lifetime as two fields: the presets, and the custom one
// behind them, which applies only while the presets are on custom. Forever is
// named by the caller: on a binding it never expires, and on a credential's
// ceiling it is no limit.
func lifetimeFields(key, label, section, forever string, presets []time.Duration, seconds int64) []Field {
	choices := make([]Choice, 0, len(presets)+1)
	at := LifetimeCustom
	for _, d := range presets {
		name := lifetime.Label(d)
		if d == lifetime.Forever {
			name = forever
		}
		k := strconv.FormatInt(lifetime.Seconds(d), 10)
		if k == strconv.FormatInt(seconds, 10) {
			at = k
		}
		choices = append(choices, Choice{Key: k, Label: name})
	}
	choices = append(choices, Choice{Key: LifetimeCustom, Label: "custom…"})
	return []Field{
		{Key: key, Label: label, Section: section, Choices: choices, Value: at},
		// The typed field reads as a continuation of the picker above it
		// rather than as a second field with the same name.
		{Key: key + "Custom", Label: "…how long", Section: section, Placeholder: "e.g. 90m, 3d, 6mo",
			Value: lifetime.Label(time.Duration(seconds) * time.Second),
			Why:   "the presets above cover it",
			When:  func(a Answers) bool { return a.Get(key) == LifetimeCustom }},
	}
}

// lifetimeSeconds reads a lifetime back off its two fields, refusing what is
// not a lifetime rather than quietly meaning something else. The typed field
// takes anything the flags take: 90m, 3d, 6mo, forever, or a bare number of
// seconds.
func lifetimeSeconds(a Answers, key string) (int64, bool) {
	chosen := a.Get(key)
	if chosen != LifetimeCustom {
		seconds, err := strconv.ParseInt(chosen, 10, 64)
		return seconds, err == nil
	}
	d, err := lifetime.Parse(a.Get(key + "Custom"))
	if err != nil {
		return 0, false
	}
	return lifetime.Seconds(d), true
}

// NewSecretAsk is what a new-secret form opens on, and what it is for.
type NewSecretAsk struct {
	// Name and Routes fill the form, as a request asks for them. A name the
	// project already holds (Secrets) opens as the first free one instead
	// ("github-2"), which the server would otherwise refuse the create for.
	Name    string
	Routes  []secretroute.Route
	Secrets []Secret
	// Approving says the secret is stored to answer a request with a binding
	// lasting BindFor (zero: forever), which a binding limit shorter than that
	// would have the server refuse after the secret was stored.
	Approving bool
	BindFor   time.Duration
}

// NewSecretForm stores a credential the project does not have yet.
func NewSecretForm(ask NewSecretAsk) Form[NewSecret] {
	fields := secretFields(nil, ask.Routes)
	for i := range fields {
		if fields[i].Key == "name" {
			fields[i].Value = freeSecretName(ask.Secrets, ask.Name)
		}
	}
	return Form[NewSecret]{Fields: fields, Submit: func(a Answers) (NewSecret, *Refusal) {
		secret, refused := newSecret(a)
		if refused != nil {
			return secret, refused
		}
		// A limit shorter than the binding this approval is making is one the
		// server would refuse the binding for, after the secret was stored.
		if limit := time.Duration(secret.MaxTTLSeconds) * time.Second; ask.Approving && limit > 0 && (ask.BindFor <= 0 || ask.BindFor > limit) {
			return secret, refuse("the binding limit is shorter than the " + lifetime.Label(ask.BindFor) + " this request is being approved for")
		}
		return secret, nil
	}}
}

// secretNameTaken reports whether the project already holds a secret by this
// name: a secret's name is unique in its project.
func secretNameTaken(secrets []Secret, name string) bool {
	name = strings.TrimSpace(name)
	for _, secret := range secrets {
		if strings.TrimSpace(secret.Name) == name {
			return true
		}
	}
	return false
}

// freeSecretName is name, or the first of name-2, name-3… the project does not
// hold. It is a suggestion to type over, not a name anything is stored under
// without being seen: one more collision than there are secrets is impossible,
// which is where the count stops.
func freeSecretName(secrets []Secret, name string) string {
	if name == "" || !secretNameTaken(secrets, name) {
		return name
	}
	for n := 2; n <= len(secrets)+2; n++ {
		candidate := name + "-" + strconv.Itoa(n)
		if !secretNameTaken(secrets, candidate) {
			return candidate
		}
	}
	return name
}

// newSecret is the secret the new-secret form says, or why it cannot be.
func newSecret(a Answers) (NewSecret, *Refusal) {
	seconds, ok := lifetimeSeconds(a, "ttl")
	if !ok {
		return NewSecret{}, refuse("a limit is 1h, 90m, 3d, 2w, 1mo, or no limit")
	}
	format, refused := sentinelFormat(a)
	if refused != nil {
		return NewSecret{}, refused
	}
	routes, refused := secretRoutes(a)
	if refused != nil {
		return NewSecret{}, refused
	}
	// A secret with no routes is sent nowhere (ADR 26-10-27-880 §1), so one
	// is asked for, as `discobox secret create` asks: `*` is how a person
	// says anywhere (ADR 26-10-27-939).
	if len(routes) == 0 {
		return NewSecret{}, refuse("say where it may be sent: a host on a route row, or * for anywhere")
	}
	secret := NewSecret{
		Name:          a.Get("name"),
		Type:          a.Get("kind"),
		MaxTTLSeconds: seconds,
		Routes:        routes,
		Format:        format,
		Value:         secretValue(a),
	}
	if secret.Type == "token" && a.Get("source") == "command" {
		command, err := refreshcmd.Split(a.Get("command"))
		if err != nil || len(command) == 0 {
			return NewSecret{}, refuse("the command is words, with quotes around one that holds a space")
		}
		secret.RefreshCommand, secret.Value = command, SecretValue{}
		lasts, ok := lifetimeSeconds(a, "lasts")
		if !ok {
			return NewSecret{}, refuse("a value lasts 5m, 90m, 8h, or never goes stale")
		}
		secret.ValueTTLSeconds = &lasts
	}
	return secret, nil
}

// EditSecretForm is the same card over a credential that already exists. Its
// answer is only what changed; an empty update is nothing to save.
//
// Everything but the kind may be told: the name, the routes, the limit, and
// the credential itself. A value cannot be read back, but it can be replaced,
// and a card that would not take one made rotating a token a matter of
// deleting the secret and storing it again, taking every binding that stood on
// it along with it.
func EditSecretForm(was Secret) Form[SecretUpdate] {
	// Each answer is compared with what its field opened holding (opening),
	// not with what was read: a stored value a field cannot hold as it is
	// would otherwise read as an edit nobody made.
	fields := secretFields(&was, was.Routes)
	opened := opening(fields)
	return Form[SecretUpdate]{Fields: fields, Submit: func(a Answers) (SecretUpdate, *Refusal) {
		seconds, ok := lifetimeSeconds(a, "ttl")
		if !ok {
			return SecretUpdate{}, refuse("a limit is 1h, 90m, 3d, 2w, 1mo, or no limit")
		}
		update := SecretUpdate{}
		if name := a.Get("name"); name != opened.Get("name") {
			update.Name = &name
		}
		if time.Duration(seconds)*time.Second != was.MaxTTL {
			update.MaxTTLSeconds = &seconds
		}
		routes, refused := secretRoutes(a)
		if refused != nil {
			return SecretUpdate{}, refused
		}
		// Measured against the route fields as they opened, read the same
		// way, so a stored route a field holds otherwise is no edit.
		if openedRoutes, _ := secretRoutes(opened); RoutesText(routes) != RoutesText(openedRoutes) {
			if routes == nil {
				routes = []secretroute.Route{}
			}
			update.Routes = &routes
		}
		format, refused := sentinelFormat(a)
		if refused != nil {
			return SecretUpdate{}, refused
		}
		if format != opened.Get("format") {
			update.Format = &format
		}
		value, refused := replacementValue(a, opened, was)
		if refused != nil {
			return SecretUpdate{}, refused
		}
		update.Value = value
		if was.Type == "token" {
			// A word holding a tab or a line break is a space in the field, so
			// resending an untouched command would rewrite it.
			if typed := a.Get("command"); typed != opened.Get("command") {
				command, err := refreshcmd.Split(typed)
				if err != nil {
					return SecretUpdate{}, refuse("the command is words, with quotes around one that holds a space")
				}
				update.RefreshCommand = &command
			}
			lasts, ok := lifetimeSeconds(a, "lasts")
			if !ok {
				return SecretUpdate{}, refuse("a value lasts 5m, 90m, 8h, or never goes stale")
			}
			if time.Duration(lasts)*time.Second != was.ValueTTL {
				update.ValueTTLSeconds = &lasts
			}
		}
		return update, nil
	}}
}

// secretFields is a credential on one card: the same fields for storing a new
// one and for editing what an existing one says about itself.
//
// Everything an OAuth credential needs is on the card from the start, not
// applying until the kind calls for it, so choosing "an OAuth credential"
// reveals nothing that was not already visible as something it would want. An
// access token stored as a token expires and stays expired, while an oauth
// secret is refreshed server-side as it ages out (ADR 0011), and the four
// fields that make that possible are the reason.
//
// routes are the routes the card opens on: an existing secret's own, or the
// ones a request asks for.
func secretFields(existing *Secret, routes []secretroute.Route) []Field {
	const what, value = "the credential", "what it is"
	editing := existing != nil
	oauth := func(a Answers) bool { return a.Get("kind") == "oauth" }
	// An exchange credential is the fields its recipe names (ADR
	// 26-10-08-452). It is only ever opened here, never chosen: its recipe
	// says where a key is sent, and is given with `discobox secret create
	// --exchange-recipe`.
	exchanged := func(a Answers) bool { return a.Get("kind") == "exchange" }
	token := func(a Answers) bool { return !oauth(a) && !exchanged(a) }

	name := Field{Key: "name", Label: "name", Section: what, Placeholder: "e.g. github",
		Hint: "how you pick it out when a request asks for a credential", Required: "a secret needs a name"}

	// The kind is the one thing an existing credential cannot be told. A token
	// and an oauth credential are stored and renewed differently, and the
	// server has no answer for a secret that changes from one to the other:
	// that is a new credential, and deleting the old one is somebody's
	// decision rather than a side effect of an edit.
	kind := Field{Key: "kind", Label: "kind", Section: what, Choices: []Choice{
		{Key: "token", Label: "a token", Hint: "one opaque string, however it is presented"},
		{Key: "oauth", Label: "an OAuth credential", Hint: "renews itself: an access token, a refresh token, and where to spend it"}}}

	routeFields := secretRouteFields(what, routes)

	// The shape of the sentinels that stand in for it, for a client that
	// checks a key's prefix. Empty is the ordinary answer: the shape is read
	// from the value, and re-read whenever the value changes.
	const formatSlots = "{charset:length} slots, charset one of digits hex HEX lower upper alnum base62 base32 base64 base64url"
	format := Field{Key: "format", Label: "sentinels look like", Section: "inside a discobox", Placeholder: "read from the value",
		Hint: formatSlots + " · e.g. sk-ant-oat01-{base64url:95}"}

	seconds := int64(0)
	if editing {
		seconds = int64(existing.MaxTTL / time.Second)
	}
	// "binding limit" rather than "bindings last": the value is a ceiling, and
	// a field reading "bindings last ‹ 1 day ›" states it as the lifetime
	// every binding gets, which is the opposite of what it does. A new
	// credential limits its bindings to nothing: a default limit is a decision
	// nobody made, expiring bindings mid-task far from this form.
	limit := lifetimeFields("ttl", "binding limit", what, "no limit", lifetime.Presets, seconds)
	limit[0].Hint = "the longest a binding of this credential may last: a longer one is refused, and nothing else about it changes"
	limit[1].Hint = limit[0].Hint

	// A token is either typed in or got from a command on this machine, which
	// is run now for the first value and offered to whoever renews it when it
	// goes stale (ADR 26-09-25-122 §2).
	fromCommand := func(a Answers) bool { return token(a) && a.Get("source") == "command" }
	source := Field{Key: "source", Label: "the value", Section: value, When: token, Why: "only for a token", Choices: []Choice{
		{Key: "value", Label: "enter a value", Hint: "type or paste it below"},
		{Key: "command", Label: "get it from a command", Hint: "run on this machine now, and again when it goes stale — with your say-so"}}}

	command := Field{Key: "command", Label: "command", Section: value, Placeholder: "e.g. gh auth token",
		Hint:     "prints the token; run without a shell, as you, on the machine renewing it",
		Required: "a token got from a command needs the command",
		When:     fromCommand, Why: "only for a token got from a command"}

	valueTTL := int64(defaultValueLifetime / time.Second)
	if editing {
		valueTTL = int64(existing.ValueTTL / time.Second)
	}
	lasts := lifetimeFields("lasts", "a value lasts", value, "never goes stale", valueLifetimes, valueTTL)
	lasts[0].Hint = "how long a value is trusted before a new one is asked for · a stale value is still used until one arrives"
	lasts[1].Hint = lasts[0].Hint
	lasts[0].When, lasts[0].Why = fromCommand, "only for a token got from a command"
	lasts[1].When = func(a Answers) bool { return fromCommand(a) && a.Get("lasts") == LifetimeCustom }

	plain := Field{Key: "token", Label: "token", Section: value, Placeholder: "the value itself", Masked: true,
		Hint: "stored encrypted, and never shown again", Required: "a secret is the value: nothing was stored",
		When: func(a Answers) bool { return token(a) && !fromCommand(a) }, Why: "only for a token typed in"}

	oauthOnly := "only for an OAuth credential"
	access := Field{Key: "access", Label: "access token", Section: value, Placeholder: "the value itself", Masked: true,
		Hint:     "the one that travels, and the one that expires · stored encrypted, and never shown again",
		Required: "an oauth credential needs an access token", When: oauth, Why: oauthOnly}
	refresh := Field{Key: "refresh", Label: "refresh token", Section: value, Placeholder: "spent to renew the access token", Masked: true,
		Hint:     "only the control plane ever spends it: it is never delivered to a discobox, and never leaves in a response",
		Required: "an oauth credential needs a refresh token", When: oauth, Why: oauthOnly}
	tokenURL := Field{Key: "tokenURL", Label: "renews at", Section: value, Placeholder: "e.g. https://console.anthropic.com/v1/oauth/token",
		Hint:     "where the refresh token is exchanged for a new access token",
		Required: "an oauth credential needs somewhere to renew", When: oauth, Why: oauthOnly}
	client := Field{Key: "client", Label: "client", Section: value, Placeholder: "client id (optional)",
		Hint: "which OAuth client the grant belongs to · leave it empty if the token endpoint does not ask", When: oauth, Why: oauthOnly}
	scopes := Field{Key: "scopes", Label: "may do", Section: value, Placeholder: "e.g. user:profile user:inference",
		Hint: "space separated, as the authorization server returned them — a client may gate features on these, so record them rather than guess",
		When: oauth, Why: oauthOnly}

	var exchangeFields []Field
	if editing {
		// A stored credential opens on what it is. Only the kind is locked; a
		// value cannot be read back, but it can be replaced, and a card that
		// showed the fields for it greyed out was telling people to delete a
		// credential and store it again to rotate a token.
		name.Value = existing.Name
		kind.Locked = true
		kind.Value = existing.Type
		if existing.Type == "exchange" {
			kind.Choices = append(kind.Choices, Choice{Key: "exchange", Label: "an exchange credential", Hint: "fields the control plane trades for a short-lived token"})
		}
		// Only a chosen shape opens filled in: one read from the value is
		// shown as what empty gives, so saving the card untouched does not
		// turn it into a choice nobody made.
		if existing.FormatSet {
			format.Value = existing.Format
			format.Hint = "empty reads it from the value · " + formatSlots
		} else if existing.Format != "" {
			format.Placeholder = "read from the value: " + existing.Format
		}
		if existing.OAuth != nil {
			tokenURL.Value = existing.OAuth.TokenURL
			client.Value = existing.OAuth.ClientID
			scopes.Value = strings.Join(existing.OAuth.Scopes, " ")
		}
		// The value is replaced whole or left alone: the server stores one
		// value per secret, so a new refresh token without the access token
		// beside it would drop the rest. Nothing is required, because leaving
		// every one of these empty is the ordinary answer — the credential
		// stays as it is.
		plain.Required, access.Required, refresh.Required, tokenURL.Required = "", "", "", ""
		// An existing token keeps its value unless one is typed; its command
		// is edited in place, and emptied to remove it.
		command.When = token
		command.Required = ""
		command.Value = refreshcmd.Join(existing.RefreshCommand)
		command.Hint = "offered to whoever renews it when it goes stale · empty removes it"
		// An existing token's lifetime is its own, command or not: one typed
		// in can go stale too.
		lasts[0].When = token
		lasts[1].When = func(a Answers) bool { return token(a) && a.Get("lasts") == LifetimeCustom }
		plain.When = token
		plain.Placeholder, access.Placeholder, refresh.Placeholder = keepStored, keepStored, keepStored
		plain.Hint = "typing here replaces the credential · leave it empty and the stored one stays"
		access.Hint = "replacing an oauth credential means both tokens and where it renews: the stored ones cannot be read back"
		refresh.Hint, tokenURL.Hint = access.Hint, access.Hint
		if existing.Exchange != nil {
			exchangeFields = exchangeFieldsOf(value, existing.Exchange.Fields, exchanged)
		}
	}
	// Where it may be sent comes straight after what it is: a secret with no
	// routes is sent nowhere, so it is the one question every new secret has
	// to answer.
	fields := append(append([]Field{name, kind}, routeFields...), limit...)
	if editing {
		fields = append(fields, plain, command)
		fields = append(fields, lasts...)
		fields = append(fields, exchangeFields...)
		return append(fields, access, refresh, tokenURL, client, scopes, format)
	}
	fields = append(fields, source, command)
	fields = append(fields, lasts...)
	fields = append(fields, plain)
	return append(fields, access, refresh, tokenURL, client, scopes, format)
}

// secretRouteFields are the fields a secret's routes are typed in: one per
// route it has, and one empty for another. A route is typed as
// secretroute.Parse reads one (secretroute.Syntax); emptying a field removes
// that route.
func secretRouteFields(section string, routes []secretroute.Route) []Field {
	fields := make([]Field, 0, len(routes)+1)
	for i := range len(routes) + 1 {
		f := Field{Key: SecretRouteKey + strconv.Itoa(i), Label: "route", Section: section,
			Placeholder: "e.g. GET api.github.com/repos/*/*/pulls/**",
			Hint:        secretroute.Syntax + " · a host is exact, *.h is beneath h, * for any host · =blocked refuses · with none it is sent nowhere"}
		if i > 0 {
			f.Label = "or route"
		}
		if i < len(routes) {
			f.Value = secretroute.Format(routes[i])
		}
		if i == len(routes) && i > 0 {
			f.Placeholder = "another route (optional)"
		}
		fields = append(fields, f)
	}
	return fields
}

// secretRoutes are the routes typed on the card, in order, empty ones dropped,
// or why one is not a route.
func secretRoutes(a Answers) ([]secretroute.Route, *Refusal) {
	var routes []secretroute.Route
	for i := 0; ; i++ {
		text, ok := a[SecretRouteKey+strconv.Itoa(i)]
		if !ok {
			return routes, nil
		}
		if text = strings.TrimSpace(text); text == "" {
			continue
		}
		route, err := secretroute.Parse(text)
		if err != nil {
			return nil, refuse(err.Error())
		}
		routes = append(routes, route)
	}
}

// exchangeFieldsOf are the fields an exchange credential's fields are replaced
// through, one per field its recipe names. None is required, since leaving
// them all empty keeps what is stored.
func exchangeFieldsOf(section string, names []string, when func(Answers) bool) []Field {
	fields := make([]Field, 0, len(names))
	for _, name := range names {
		fields = append(fields, Field{Key: ExchangeFieldPrefix + name, Label: name, Section: section, Masked: true,
			Placeholder: keepStored, When: when, Why: "only for an exchange credential",
			Hint: "exchanged for a short-lived token by the control plane, which is all a discobox ever holds · typing here replaces what is stored"})
	}
	return fields
}

// secretValue is the credential's material as the card holds it. An oauth
// credential is the several fields the control plane renews with; a token is
// the one field that travels.
func secretValue(a Answers) SecretValue {
	switch a.Get("kind") {
	case "exchange":
		fields := map[string]string{}
		for key := range a {
			if name, ok := strings.CutPrefix(key, ExchangeFieldPrefix); ok {
				if v := a.Get(key); v != "" {
					fields[name] = v
				}
			}
		}
		return SecretValue{Exchange: fields}
	case "oauth":
		return SecretValue{
			Token:        a.Get("access"),
			RefreshToken: a.Get("refresh"),
			TokenURL:     a.Get("tokenURL"),
			ClientID:     a.Get("client"),
			Scopes:       strings.Fields(a.Get("scopes")),
		}
	}
	return SecretValue{Token: a.Get("token")}
}

// sentinelFormat is the sentinel format the card asks for, empty for "read it
// from the value". It is checked with the parser the server uses, so a
// mistyped template is refused on the card that holds it rather than after the
// card has closed.
func sentinelFormat(a Answers) (string, *Refusal) {
	format := a.Get("format")
	if format == "" {
		return "", nil
	}
	if _, err := secretformat.ParseChosen(format); err != nil {
		return "", refuse("the sentinel format does not read (" + err.Error() + ") · it is text with {charset:length} slots")
	}
	return format, nil
}

// replacementValue is the credential the card replaces the stored one with,
// nil when it replaces nothing, and why when it cannot.
//
// The server stores one value per secret and replaces it whole, so half an
// oauth credential is not a smaller change — it is the rest of it dropped.
// Touching any part of one therefore means typing all of it.
func replacementValue(a, opened Answers, was Secret) (*SecretValue, *Refusal) {
	value := secretValue(a)
	if was.Type == "exchange" {
		// Replaced whole, like an oauth credential: every field, or none.
		var missing []string
		if was.Exchange != nil {
			for _, name := range was.Exchange.Fields {
				if value.Exchange[name] == "" {
					missing = append(missing, name)
				}
			}
		}
		switch {
		case len(value.Exchange) == 0:
			return nil, nil
		case len(missing) > 0:
			return nil, refuse("replacing it means every field: " + strings.Join(missing, ", ") + " too, since the stored ones cannot be read back")
		}
		return &value, nil
	}
	if was.Type != "oauth" {
		if value.Token == "" {
			return nil, nil
		}
		return &value, nil
	}
	// Measured against what the fields opened holding, so a stored value a
	// field cannot hold as it is does not read as a renewal changed.
	renewalChanged := value.TokenURL != opened.Get("tokenURL") ||
		value.ClientID != opened.Get("client") ||
		strings.Join(value.Scopes, " ") != strings.Join(strings.Fields(opened.Get("scopes")), " ")
	if value.Token == "" && value.RefreshToken == "" && !renewalChanged {
		return nil, nil
	}
	switch {
	case value.Token == "":
		return nil, refuse("replacing an oauth credential means its access token too: the stored one cannot be read back")
	case value.RefreshToken == "":
		return nil, refuse("replacing an oauth credential means its refresh token too: the stored one cannot be read back")
	case value.TokenURL == "":
		return nil, refuse("an oauth credential needs somewhere to renew")
	}
	return &value, nil
}

// BindingForm binds secret by hand: what it is on, what for, as which
// variable, where it may be sent and for how long, read against each other
// on one card. harnesses are the project's; boxes are the discoboxes on the
// secret's own server, since one on another server is not one it can reach.
func BindingForm(secret Secret, harnesses []Harness, boxes []Sandbox) Form[NewBinding] {
	targets := []Choice{{Key: BindingProject, Label: "every discobox in this project",
		Hint: "the widest of the three: anything the project runs gets the credential"}}
	if len(harnesses) > 0 {
		targets = append(targets, Choice{Key: BindingHarnessConfig, Label: "one harness",
			Hint: "every discobox running that harness gets the credential"})
	}
	boxChoices := make([]Choice, 0, len(boxes))
	for _, box := range boxes {
		boxChoices = append(boxChoices, Choice{Key: box.ID, Label: box.Name, Hint: box.ID})
	}
	if len(boxChoices) > 0 {
		targets = append(targets, Choice{Key: BindingSandbox, Label: "one discobox",
			Hint: "the narrowest, and what approving a request makes"})
	}
	harnessChoices := make([]Choice, 0, len(harnesses))
	for _, h := range harnesses {
		harnessChoices = append(harnessChoices, Choice{Key: h.ID, Label: h.Name, Hint: h.ID})
	}

	const who, what, where = "what gets it", "what it is for", "where, and for how long"
	uses := func(a Answers) bool { return a.Get("purpose") != "delegate" }
	fields := []Field{
		{Key: "target", Label: "bound to", Section: who, Choices: targets},
		{Key: "harness", Label: "harness", Section: who, Choices: harnessChoices,
			When: func(a Answers) bool { return a.Get("target") == BindingHarnessConfig }, Why: "only when it is bound to one harness"},
		{Key: "sandbox", Label: "discobox", Section: who, Choices: boxChoices,
			When: func(a Answers) bool { return a.Get("target") == BindingSandbox }, Why: "only when it is bound to one discobox"},
		{Key: "purpose", Label: "purpose", Section: what, Choices: []Choice{
			{Key: "use", Label: "to use it", Hint: "the discobox finds a sentinel for it in the variable below"},
			{Key: "delegate", Label: "to give it on", Hint: "the discobox may bind it to the discoboxes it leads, and never uses it itself"},
			{Key: "both", Label: "to use it and give it on", Hint: "both of the above"}}},
		{Key: "envVar", Label: "as variable", Section: what, Placeholder: "e.g. GITHUB_TOKEN", Value: secretenv.Name(secret.Name),
			Hint: "the environment variable the discobox finds the credential's sentinel in; one variable holds one credential",
			When: uses, Why: "a credential only given on is in no variable"},
	}
	routes := secretRouteFields(where, nil)
	for i := range routes {
		routes[i].Hint = "narrows where the secret's own routes send it, for this binding alone · empty keeps the secret's: " + RoutesText(secret.Routes)
	}
	ttl := lifetimeFields("ttl", "lifetime", where, "never expires", lifetime.Presets, int64(secret.MaxTTL/time.Second))
	lasts := "it never expires: a credential nobody has to think about again, and one nothing takes away"
	if secret.MaxTTL > 0 {
		lasts = "this credential allows at most " + lifetime.Label(secret.MaxTTL) + ", so it cannot be bound forever"
	}
	ttl[0].Hint, ttl[1].Hint = lasts, lasts
	fields = append(append(fields, routes...), ttl...)

	return Form[NewBinding]{Fields: fields, Submit: func(a Answers) (NewBinding, *Refusal) {
		seconds, ok := lifetimeSeconds(a, "ttl")
		if !ok {
			return NewBinding{}, refuse("a lifetime is 1h, 90m, 3d, 2w, 1mo, or forever")
		}
		routes, refused := secretRoutes(a)
		if refused != nil {
			return NewBinding{}, refused
		}
		purpose := a.Get("purpose")
		binding := NewBinding{
			SecretID:   secret.ID,
			TargetKind: a.Get("target"),
			Use:        purpose != "delegate",
			Delegate:   purpose != "use",
			Routes:     routes,
			TTLSeconds: seconds,
		}
		if binding.Use {
			binding.EnvVar = a.Get("envVar")
			if binding.EnvVar == "" {
				return NewBinding{}, refuse("a credential to use needs a variable to be found in")
			}
		}
		switch binding.TargetKind {
		case BindingHarnessConfig:
			binding.TargetID = a.Get("harness")
		case BindingSandbox:
			binding.TargetID = a.Get("sandbox")
		}
		return binding, nil
	}}
}
