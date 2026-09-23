package merge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
)

type MergeError struct {
	File    string
	Path    string
	Message string
	Cause   error
}

func (e *MergeError) Error() string {
	var b strings.Builder
	b.WriteString(e.Message)
	if e.File != "" {
		b.WriteString(" (in ")
		b.WriteString(e.File)
		if e.Path != "" {
			b.WriteString(" at ")
			b.WriteString(e.Path)
		}
		b.WriteString(")")
	}
	return b.String()
}

func (e *MergeError) Unwrap() error {
	return e.Cause
}

// OpenAPI models the root fields of an OpenAPI document. Nested content
// (Info, Paths, Webhooks, Components, and each entry of
// Servers/Security/Tags) is represented generically as yaml.MapSlice/[]any
// rather than further-typed structs, so that fields within them the
// merger doesn't process directly — like OpenAPI 3.2's tags[].parent, or
// a tag's description/externalDocs — still round-trip unchanged.
//
// Root-level fields not listed here — OpenAPI 3.1's "jsonSchemaDialect",
// 3.2's "$self"/"summary", "externalDocs", and vendor "x-" extensions
// aren't modeled as struct fields, but OapiYaml still preserves them
// verbatim; see extraRootFields.
type OpenAPI struct {
	OpenAPI    string        `yaml:"openapi"`
	Info       yaml.MapSlice `yaml:"info"`
	Servers    []any         `yaml:"servers,omitempty"`
	Paths      yaml.MapSlice `yaml:"paths"`
	Webhooks   yaml.MapSlice `yaml:"webhooks,omitempty"`
	Components yaml.MapSlice `yaml:"components,omitempty"`
	Security   []any         `yaml:"security,omitempty"`
	Tags       []any         `yaml:"tags,omitempty"`
}

func OapiYaml(inputFile, outputFile string) error {
	data, err := os.ReadFile(inputFile)
	if err != nil {
		return &MergeError{File: inputFile, Message: "Failed to read input file", Cause: err}
	}

	var mainAPI OpenAPI
	if err := yaml.UnmarshalWithOptions(data, &mainAPI, yaml.UseOrderedMap()); err != nil {
		return &MergeError{File: inputFile, Message: "Invalid OpenAPI YAML structure", Cause: err}
	}

	if mainAPI.OpenAPI == "" {
		return &MergeError{File: inputFile, Message: "Missing required field 'openapi'"}
	}
	if len(mainAPI.Info) == 0 {
		return &MergeError{File: inputFile, Message: "Missing required field 'info'"}
	}

	// None of these can contain a $ref this merger needs to resolve, so
	// they're extracted once up front and reattached after marshaling,
	// rather than threaded through the struct or its $ref processing.
	extra, err := extraRootFields(data)
	if err != nil {
		return &MergeError{File: inputFile, Message: "Invalid OpenAPI YAML structure", Cause: err}
	}

	urlsToParse := make(map[string]bool)
	if err := processPathItemMap(&mainAPI.Paths, urlsToParse, inputFile); err != nil {
		return err
	}
	if err := processPathItemMap(&mainAPI.Webhooks, urlsToParse, inputFile); err != nil {
		return err
	}

	if err := processNestedFiles(urlsToParse, &mainAPI.Components); err != nil {
		return err
	}

	if err := validateNoDanglingLocalRefs(&mainAPI, inputFile); err != nil {
		return err
	}

	data, err = yaml.MarshalWithOptions(&mainAPI, yaml.Indent(2), yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		return fmt.Errorf("failed to marshal YAML: %w", err)
	}

	if len(extra) > 0 {
		data, err = appendExtraRootFields(data, extra)
		if err != nil {
			return fmt.Errorf("failed to marshal YAML: %w", err)
		}
	}

	return os.WriteFile(outputFile, data, 0644)
}

// namedExtraRootFields lists the additional OpenAPI 3.1/3.2 root fields
// OapiYaml preserves verbatim even though they aren't modeled by the
// OpenAPI struct: "jsonSchemaDialect" (3.1), and "$self"/"summary"/
// "externalDocs" (3.2 adds "$self"/"summary"; "externalDocs" predates
// 3.1 but was likewise never modeled). None of these can contain a $ref
// this merger resolves, so they need no special processing beyond
// round-tripping unchanged.
var namedExtraRootFields = map[string]bool{
	"jsonSchemaDialect": true,
	"$self":             true,
	"summary":           true,
	"externalDocs":      true,
}

// extraRootFields re-parses data generically to pull out the root fields
// in namedExtraRootFields, plus any vendor "x-" extension, in their
// original relative order. OapiYaml reattaches these to its own marshal
// output via appendExtraRootFields.
func extraRootFields(data []byte) (yaml.MapSlice, error) {
	var raw yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &raw, yaml.UseOrderedMap()); err != nil {
		return nil, err
	}

	var extra yaml.MapSlice
	for _, item := range raw {
		key, ok := item.Key.(string)
		if !ok {
			continue
		}
		if namedExtraRootFields[key] || strings.HasPrefix(key, "x-") {
			extra = append(extra, item)
		}
	}
	return extra, nil
}

// appendExtraRootFields re-parses marshaled (OapiYaml's own marshal
// output for the OpenAPI struct) into a MapSlice, appends extra's items
// after the fields already there, and re-marshals.
func appendExtraRootFields(marshaled []byte, extra yaml.MapSlice) ([]byte, error) {
	var doc yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(marshaled, &doc, yaml.UseOrderedMap()); err != nil {
		return nil, err
	}
	doc = append(doc, extra...)
	return yaml.MarshalWithOptions(doc, yaml.Indent(2), yaml.UseLiteralStyleIfMultiline(true))
}

// processPathItemMap resolves whole-item "$ref"s in a map of Path Item
// Objects, fetching and inlining the referenced content. It is used for
// both Paths and Webhooks (OpenAPI 3.1+), since both fields share the
// same "name -> Path Item Object" shape.
func processPathItemMap(paths *yaml.MapSlice, urlsToParse map[string]bool, currentFilePath string) error {
	for i := range *paths {
		pathKey := (*paths)[i].Key.(string)
		pathValue := (*paths)[i].Value

		pathMap, ok := pathValue.(yaml.MapSlice)
		if !ok {
			continue
		}

		refValue := getMapSliceValue(pathMap, "$ref")
		refStr, isExternalRef := refValue.(string)
		isExternalRef = isExternalRef && !strings.HasPrefix(refStr, "#")

		if !isExternalRef {
			// Not a whole-item external $ref: this is an inline path/webhook
			// item, which may still contain external $refs nested inside its
			// operations (e.g. a response or request body schema). Those need
			// the same treatment findRefs already gives fetched content below.
			findRefs(&pathMap, urlsToParse, currentFilePath)
			(*paths)[i].Value = pathMap
			continue
		}

		parts := strings.SplitN(refStr, "#", 2)
		if len(parts) < 2 {
			return &MergeError{File: currentFilePath, Path: pathKey, Message: fmt.Sprintf("Invalid $ref format '%s': missing fragment", refStr)}
		}

		refPath := resolveRef(parts[0], currentFilePath)
		urlsToParse[refPath] = true

		fragment := parts[1]
		if !strings.HasPrefix(fragment, "/") {
			fragment = "/" + fragment
		}

		data, err := os.ReadFile(refPath)
		if err != nil {
			return &MergeError{File: currentFilePath, Path: pathKey, Message: fmt.Sprintf("Cannot read referenced file '%s'", refPath), Cause: err}
		}

		var nested yaml.MapSlice
		if err := yaml.UnmarshalWithOptions(data, &nested, yaml.UseOrderedMap()); err != nil {
			return &MergeError{File: refPath, Message: "Invalid YAML syntax", Cause: err}
		}

		current, err := navigateToFragment(nested, fragment, refPath)
		if err != nil {
			return err
		}

		resolvedPathItem, ok := current.(yaml.MapSlice)
		if !ok {
			return &MergeError{File: refPath, Path: fragment, Message: "Invalid reference target"}
		}

		findRefs(&resolvedPathItem, urlsToParse, refPath)
		(*paths)[i].Value = resolvedPathItem
	}
	return nil
}

func navigateToFragment(nested yaml.MapSlice, fragment, refPath string) (any, error) {
	var current any = nested
	for _, part := range strings.Split(strings.TrimPrefix(fragment, "/"), "/") {
		if part == "" {
			continue
		}
		currentMap, ok := current.(yaml.MapSlice)
		if !ok {
			return nil, &MergeError{File: refPath, Path: fragment, Message: "Invalid reference structure"}
		}
		value := getMapSliceValue(currentMap, part)
		if value == nil {
			return nil, &MergeError{File: refPath, Path: fragment, Message: fmt.Sprintf("Key '%s' not found", part)}
		}
		current = value
	}
	return current, nil
}

func processNestedFiles(urlsToParse map[string]bool, components *yaml.MapSlice) error {
	componentTypes := []string{
		"schemas",
		"responses",
		"parameters",
		"examples",
		"requestBodies",
		"headers",
		"securitySchemes",
		"links",
		"callbacks",
	}

	processed := make(map[string]bool)

	for {
		var pending []string
		for url := range urlsToParse {
			if !processed[url] {
				pending = append(pending, url)
			}
		}

		if len(pending) == 0 {
			break
		}

		for _, url := range pending {
			processed[url] = true

			data, err := os.ReadFile(url)
			if err != nil {
				return fmt.Errorf("failed to read '%s': %w", url, err)
			}

			var nested yaml.MapSlice
			if err := yaml.UnmarshalWithOptions(data, &nested, yaml.UseOrderedMap()); err != nil {
				return fmt.Errorf("failed to parse '%s': %w", url, err)
			}

			if nestedComponents := getMapSliceValue(nested, "components"); nestedComponents != nil {
				if compMap, ok := nestedComponents.(yaml.MapSlice); ok {
					// Merge every category actually present under this file's
					// "components:", not just componentTypes below. Since this
					// content is already unambiguously nested under
					// "components:", there's no need to guess at recognized
					// category names to include it — this covers any current
					// or future OpenAPI component category (e.g. "pathItems")
					// without the merger needing to hard-code it.
					mergeComponents(
						compMap,
						components,
						mapSliceKeys(compMap),
						urlsToParse,
						url,
					)
				}
			}

			for _, ct := range componentTypes {
				if getMapSliceValue(nested, ct) != nil {
					mergeComponents(
						nested,
						components,
						componentTypes,
						urlsToParse,
						url,
					)
					break
				}
			}
		}
	}
	return nil
}

// mapSliceKeys returns the string keys of m, in order.
func mapSliceKeys(m yaml.MapSlice) []string {
	keys := make([]string, 0, len(m))
	for _, item := range m {
		if key, ok := item.Key.(string); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

func mergeComponents(
	nestedComponents yaml.MapSlice,
	components *yaml.MapSlice,
	componentTypes []string,
	urlsToParse map[string]bool,
	currentFilePath string,
) {
	for _, compType := range componentTypes {
		nestedComp, ok := getMapSliceValue(nestedComponents, compType).(yaml.MapSlice)
		if !ok {
			continue
		}

		mainComp, _ := getMapSliceValue(*components, compType).(yaml.MapSlice)
		componentsToMerge := make(yaml.MapSlice, 0, len(nestedComp))
		for _, item := range nestedComp {
			if getMapSliceValue(mainComp, item.Key.(string)) == nil {
				componentsToMerge = append(componentsToMerge, item)
			}
		}

		findRefs(&componentsToMerge, urlsToParse, currentFilePath)
		mainComp = append(mainComp, componentsToMerge...)
		setMapSliceValue(components, compType, mainComp)
	}
}

func findRefs(api *yaml.MapSlice, urlsToParse map[string]bool, currentFilePath string) {
	for i := range *api {
		key := (*api)[i].Key.(string)
		value := (*api)[i].Value

		if key == "$ref" {
			if refStr, ok := value.(string); ok && strings.Contains(refStr, "#") && !strings.HasPrefix(refStr, "#") {
				parts := strings.SplitN(refStr, "#", 2)
				if urlsToParse != nil {
					urlsToParse[resolveRef(parts[0], currentFilePath)] = true
				}
				(*api)[i].Value = "#" + parts[1]
			}
		} else {
			processValue(value, urlsToParse, currentFilePath)
		}
	}
}

func processValue(v any, urlsToParse map[string]bool, currentFilePath string) {
	switch vt := v.(type) {
	case yaml.MapSlice:
		findRefs(&vt, urlsToParse, currentFilePath)
	case []any:
		for _, item := range vt {
			processValue(item, urlsToParse, currentFilePath)
		}
	}
}

func getMapSliceValue(m yaml.MapSlice, key string) any {
	for _, item := range m {
		if item.Key == key {
			return item.Value
		}
	}
	return nil
}

func setMapSliceValue(m *yaml.MapSlice, key string, value any) {
	for i := range *m {
		if (*m)[i].Key == key {
			(*m)[i].Value = value
			return
		}
	}
	*m = append(*m, yaml.MapItem{Key: key, Value: value})
}

// validateNoDanglingLocalRefs returns an error if any local "#/..." $ref
// in mainAPI's Paths, Webhooks, or Components doesn't resolve to a value
// that actually exists in the merged output.
//
// findRefs rewrites external $refs to local ones optimistically, before
// the corresponding content has necessarily been imported — for example
// if the referenced file's content isn't nested under a "components:"
// object, or under a recognized component category, findRefs still
// rewrites the reference, but nothing ever copies the target into the
// output. This pass turns that otherwise-silent dangling reference into
// an explicit, actionable error instead.
func validateNoDanglingLocalRefs(mainAPI *OpenAPI, inputFile string) error {
	// A minimal stand-in for the merged document, containing just the
	// sections findRefs actually rewrites references within/into.
	root := yaml.MapSlice{
		{Key: "paths", Value: mainAPI.Paths},
		{Key: "webhooks", Value: mainAPI.Webhooks},
		{Key: "components", Value: mainAPI.Components},
	}

	var fragments []string
	collectLocalRefs(root, &fragments)

	for _, fragment := range fragments {
		if _, err := navigateToFragment(root, fragment, inputFile); err != nil {
			return &MergeError{File: inputFile, Path: fragment, Message: fmt.Sprintf("Reference '#%s' does not resolve to a merged value", fragment)}
		}
	}
	return nil
}

// collectLocalRefs appends the fragment (including its leading "/") of
// every local "$ref: '#/...'" found anywhere under v to *fragments.
func collectLocalRefs(v any, fragments *[]string) {
	switch vt := v.(type) {
	case yaml.MapSlice:
		for _, item := range vt {
			key, _ := item.Key.(string)
			if key == "$ref" {
				if refStr, ok := item.Value.(string); ok && strings.HasPrefix(refStr, "#/") {
					*fragments = append(*fragments, strings.TrimPrefix(refStr, "#"))
				}
				continue
			}
			collectLocalRefs(item.Value, fragments)
		}
	case []any:
		for _, item := range vt {
			collectLocalRefs(item, fragments)
		}
	}
}

func resolveRef(relativePath, currentFilePath string) string {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return relativePath
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFilePath), relativePath))
}
