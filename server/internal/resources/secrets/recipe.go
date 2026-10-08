package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// This file reads an exchange recipe (ADR 26-10-08-452): whether one is sound,
// the request it makes from what was stored, and the token in its answer.

// checkRecipe reports what is wrong with a recipe for a secret bound to host.
//
// The URL is where the long-lived key is sent, so it is held to the binding
// that already says where the token may go: a key is never sent anywhere its
// token could not be. A template naming a field the recipe does not declare
// would send the placeholder itself, and a field nothing sends is a key stored
// for no reason.
func checkRecipe(recipe *model.ExchangeRecipe, host string) error {
	var problems []string
	u, err := url.Parse(recipe.URL)
	switch {
	case err != nil || u.Scheme != "https" || u.Hostname() == "":
		problems = append(problems, fmt.Sprintf("url %q is not an https URL", recipe.URL))
	case host == "":
		problems = append(problems, "an exchange secret is bound to a host, and its url must sit inside it: give it one")
	case !hostscope.Covers(host, u.Hostname()):
		problems = append(problems, fmt.Sprintf("url host %s is outside the secret's binding %s: a key is only sent where its token may go", u.Hostname(), host))
	}
	if len(recipe.Fields) == 0 {
		problems = append(problems, "no fields")
	}
	declared := map[string]bool{}
	for _, name := range recipe.Fields {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "{}") || declared[name] {
			problems = append(problems, fmt.Sprintf("field %q is empty, braced, or repeated", name))
		}
		declared[name] = true
	}
	used := map[string]bool{}
	for _, template := range templates(recipe) {
		for rest := template; ; {
			open := strings.IndexByte(rest, '{')
			if open < 0 {
				break
			}
			end := strings.IndexByte(rest[open:], '}')
			if end < 0 {
				break
			}
			name := rest[open+1 : open+end]
			if !declared[name] {
				problems = append(problems, fmt.Sprintf("a template names %q, which is not a field", name))
			}
			used[name] = true
			rest = rest[open+end+1:]
		}
	}
	for _, name := range recipe.Fields {
		if !used[name] {
			problems = append(problems, fmt.Sprintf("field %q is never sent", name))
		}
	}
	if strings.TrimSpace(recipe.TokenPath) == "" {
		problems = append(problems, "no tokenPath")
	}
	if recipe.ExpiresAtPath != "" && recipe.ExpiresInPath != "" {
		problems = append(problems, "both expiresAtPath and expiresInPath")
	}
	if len(problems) == 0 {
		return nil
	}
	slices.Sort(problems)
	return fmt.Errorf("the exchange recipe is not usable: %s", strings.Join(slices.Compact(problems), "; "))
}

// templates are every string a recipe fills from the stored fields.
func templates(recipe *model.ExchangeRecipe) []string {
	var all []string
	for _, v := range recipe.Body {
		all = append(all, v)
	}
	for _, v := range recipe.Header {
		all = append(all, v)
	}
	return all
}

// missingFields are the recipe's fields that stored leaves out or empty.
func missingFields(recipe *model.ExchangeRecipe, stored map[string]string) []string {
	var missing []string
	for _, name := range recipe.Fields {
		if strings.TrimSpace(stored[name]) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// recipeRequest builds the exchange request from what was stored. It refuses
// one missing a field, so a template never sends a literal "{api_key}". A
// stored value is put in as data: JSON-encoded in a JSON body, form-encoded
// in a form one, so a key holding a quote stays one string.
func recipeRequest(ctx context.Context, recipe *model.ExchangeRecipe, stored map[string]string) (*http.Request, error) {
	if missing := missingFields(recipe, stored); len(missing) > 0 {
		return nil, fmt.Errorf("exchange needs %s", strings.Join(missing, ", "))
	}
	pairs := make([]string, 0, 2*len(recipe.Fields))
	for _, name := range recipe.Fields {
		pairs = append(pairs, "{"+name+"}", stored[name])
	}
	fill := strings.NewReplacer(pairs...)
	var (
		payload     []byte
		contentType string
	)
	if recipe.Form {
		form := url.Values{}
		for key, value := range recipe.Body {
			form.Set(key, fill.Replace(value))
		}
		payload, contentType = []byte(form.Encode()), "application/x-www-form-urlencoded"
	} else {
		body := make(map[string]string, len(recipe.Body))
		for key, value := range recipe.Body {
			body[key] = fill.Replace(value)
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload, contentType = encoded, "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, recipe.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	for key, value := range recipe.Header {
		req.Header.Set(key, fill.Replace(value))
	}
	return req, nil
}

// recipeToken reads the token and its expiry out of the endpoint's JSON
// answer. A zero expiry is one the answer did not say and the token does not
// carry.
func recipeToken(recipe *model.ExchangeRecipe, answer []byte) (string, time.Time, error) {
	var doc any
	if err := json.Unmarshal(answer, &doc); err != nil {
		return "", time.Time{}, fmt.Errorf("exchange answer is not JSON: %w", err)
	}
	token, _ := jsonPath(doc, recipe.TokenPath).(string)
	if strings.TrimSpace(token) == "" {
		return "", time.Time{}, fmt.Errorf("exchange answer has no token at %q", recipe.TokenPath)
	}
	var expiry time.Time
	switch {
	case recipe.ExpiresAtPath != "":
		if at, ok := jsonPath(doc, recipe.ExpiresAtPath).(float64); ok && at > 0 {
			expiry = time.Unix(int64(at), 0).UTC()
		}
	case recipe.ExpiresInPath != "":
		if in, ok := jsonPath(doc, recipe.ExpiresInPath).(float64); ok && in > 0 {
			expiry = time.Now().UTC().Add(time.Duration(in) * time.Second)
		}
	}
	if expiry.IsZero() {
		if exp := jwtExpiryMillis(token); exp > 0 {
			expiry = time.UnixMilli(exp).UTC()
		}
	}
	return token, expiry, nil
}

// jsonPath walks a dot-separated path of object keys through a decoded JSON
// document, and is nil where the path leads nowhere.
func jsonPath(doc any, path string) any {
	for _, key := range strings.Split(path, ".") {
		object, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		doc = object[key]
	}
	return doc
}
