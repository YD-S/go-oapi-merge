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

// topLevelFieldOrder defines the canonical ordering of the well-known
// OpenAPI root fields in the merged output. The document is otherwise
// parsed generically (see OapiYaml) so that any other root-level field,
// including OpenAPI 3.1/3.2 additions such as "webhooks",
// "jsonSchemaDialect", "$self", "summary", or vendor "x-" extensions,
// is preserved verbatim, in its original relative order, after these fields
// instead of being silently dropped.
var topLevelFieldOrder = []string{"openapi", "info", "servers", "paths", "components", "security", "tags"}

func OapiYaml(inputFile, outputFile string) error {
	data, err := os.ReadFile(inputFile)
	if err != nil {
		return &MergeError{File: inputFile, Message: "Failed to read input file", Cause: err}
	}

	var doc yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.UseOrderedMap()); err != nil {
		return &MergeError{File: inputFile, Message: "Invalid OpenAPI YAML structure", Cause: err}
	}

	openapiVersion, _ := getMapSliceValue(doc, "openapi").(string)
	if openapiVersion == "" {
		return &MergeError{File: inputFile, Message: "Missing required field 'openapi'"}
	}
	info, _ := getMapSliceValue(doc, "info").(yaml.MapSlice)
	if len(info) == 0 {
		return &MergeError{File: inputFile, Message: "Missing required field 'info'"}
	}

	servers := getMapSliceValue(doc, "servers")
	security := getMapSliceValue(doc, "security")
	tags := getMapSliceValue(doc, "tags")
	paths, _ := getMapSliceValue(doc, "paths").(yaml.MapSlice)
	components, _ := getMapSliceValue(doc, "components").(yaml.MapSlice)
	extra := extraTopLevelFields(doc)

	urlsToParse := make(map[string]bool)
	if err := processPaths(&paths, urlsToParse, inputFile); err != nil {
		return err
	}

	if err := processNestedFiles(urlsToParse, &components); err != nil {
		return err
	}

	merged := make(yaml.MapSlice, 0, len(topLevelFieldOrder)+len(extra))
	merged = append(merged, yaml.MapItem{Key: "openapi", Value: openapiVersion})
	merged = append(merged, yaml.MapItem{Key: "info", Value: info})
	if isNonEmptySequence(servers) {
		merged = append(merged, yaml.MapItem{Key: "servers", Value: servers})
	}
	merged = append(merged, yaml.MapItem{Key: "paths", Value: paths})
	if len(components) > 0 {
		merged = append(merged, yaml.MapItem{Key: "components", Value: components})
	}
	if isNonEmptySequence(security) {
		merged = append(merged, yaml.MapItem{Key: "security", Value: security})
	}
	if isNonEmptySequence(tags) {
		merged = append(merged, yaml.MapItem{Key: "tags", Value: tags})
	}
	merged = append(merged, extra...)

	data, err = yaml.MarshalWithOptions(merged, yaml.Indent(2), yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		return fmt.Errorf("failed to marshal YAML: %w", err)
	}
	return os.WriteFile(outputFile, data, 0644)
}

// extraTopLevelFields returns the root-level entries of doc that are not
// part of topLevelFieldOrder, preserving their original relative order.
func extraTopLevelFields(doc yaml.MapSlice) yaml.MapSlice {
	known := make(map[string]bool, len(topLevelFieldOrder))
	for _, k := range topLevelFieldOrder {
		known[k] = true
	}

	var extra yaml.MapSlice
	for _, item := range doc {
		key, ok := item.Key.(string)
		if !ok || known[key] {
			continue
		}
		extra = append(extra, item)
	}
	return extra
}

func isNonEmptySequence(v any) bool {
	s, ok := v.([]any)
	return ok && len(s) > 0
}

func processPaths(paths *yaml.MapSlice, urlsToParse map[string]bool, currentFilePath string) error {
	for i := range *paths {
		pathKey := (*paths)[i].Key.(string)
		pathValue := (*paths)[i].Value

		pathMap, ok := pathValue.(yaml.MapSlice)
		if !ok {
			continue
		}

		refValue := getMapSliceValue(pathMap, "$ref")
		if refValue == nil {
			continue
		}

		refStr, ok := refValue.(string)
		if !ok || strings.HasPrefix(refStr, "#") {
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
					mergeComponents(
						compMap,
						components,
						componentTypes,
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

func resolveRef(relativePath, currentFilePath string) string {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return relativePath
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFilePath), relativePath))
}
