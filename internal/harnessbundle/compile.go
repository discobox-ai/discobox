package harnessbundle

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// relativeTS is a relative import specifier naming a TypeScript module, as
// the sources write them (repostd's import/extensions rule): compiled, each
// names the ES module compiled beside it.
var relativeTS = regexp.MustCompile(`(["'])(\.\.?/[^"'\n]*)\.(m?)ts(["'])`)

// Compile compiles one TypeScript source of the harness tree to the ES module
// a bundle carries, file by file: types are erased and nothing is bundled, so
// what a box runs is the source's own code, under node's ES module loader.
// The harness tree's tsc checks the types; Compile only refuses what does not
// parse. The same source compiles to the same bytes.
func Compile(name string, source []byte) ([]byte, error) {
	result := api.Transform(string(source), api.TransformOptions{
		Loader:     api.LoaderTS,
		Format:     api.FormatESModule,
		Platform:   api.PlatformNode,
		Target:     api.ES2023,
		Sourcefile: name,
		// Type-only imports go, and every other import stays, as tsc's
		// verbatimModuleSyntax has it.
		TsconfigRaw:   `{"compilerOptions":{"verbatimModuleSyntax":true}}`,
		Charset:       api.CharsetUTF8,
		LegalComments: api.LegalCommentsInline,
	})
	if len(result.Errors) > 0 {
		var errs []error
		for _, m := range api.FormatMessages(result.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage}) {
			errs = append(errs, errors.New(strings.TrimSpace(m)))
		}
		return nil, fmt.Errorf("compile %s: %w", name, errors.Join(errs...))
	}
	code := relativeTS.ReplaceAllString(string(result.Code), "$1$2.mjs$4")
	header := fmt.Sprintf("// Compiled from harness/%s by discobox-harness-bundle; edit that.\n", name)
	return []byte(header + code), nil
}

// compiledName is the ES module a TypeScript source compiles to.
func compiledName(source string) string {
	return strings.TrimSuffix(source, path.Ext(source)) + ".mjs"
}
