package merge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestOapiYaml(t *testing.T) {
	t.Run("basic merge", func(t *testing.T) {
		tmpDir := t.TempDir()
		input := filepath.Join(tmpDir, "api.yaml")
		output := filepath.Join(tmpDir, "out.yaml")

		writeFile(t, input, `
openapi: "3.0.0"
info:
  title: Test
  version: "1.0"
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
`)
		if err := OapiYaml(input, output); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if _, err := os.Stat(output); err != nil {
			t.Fatal("output file not created")
		}
	})

	t.Run("with external ref", func(t *testing.T) {
		tmpDir := t.TempDir()
		input := filepath.Join(tmpDir, "api.yaml")
		paths := filepath.Join(tmpDir, "paths.yaml")
		output := filepath.Join(tmpDir, "out.yaml")

		writeFile(t, paths, `
test:
  get:
    responses:
      "200":
        description: OK
      "400":
        description: Bad
      "500":
        description: Error
`)
		writeFile(t, input, `
openapi: "3.0.0"
info:
  title: Test
  version: "1.0"
paths:
  /test:
    $ref: './paths.yaml#/test'
`)
		if err := OapiYaml(input, output); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, _ := os.ReadFile(output)
		content := string(data)

		idx200 := strings.Index(content, "200")
		idx400 := strings.Index(content, "400")
		idx500 := strings.Index(content, "500")

		if idx200 > idx400 || idx400 > idx500 {
			t.Error("response order not preserved")
		}
	})

	t.Run("missing input file", func(t *testing.T) {
		err := OapiYaml("nonexistent.yaml", "out.yaml")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "Failed to read") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("missing openapi field", func(t *testing.T) {
		tmpDir := t.TempDir()
		input := filepath.Join(tmpDir, "api.yaml")

		writeFile(t, input, `
info:
  title: Test
  version: "1.0"
paths: {}
`)
		err := OapiYaml(input, filepath.Join(tmpDir, "out.yaml"))
		if err == nil || !strings.Contains(err.Error(), "openapi") {
			t.Errorf("expected openapi error, got: %v", err)
		}
	})

	t.Run("missing info field", func(t *testing.T) {
		tmpDir := t.TempDir()
		input := filepath.Join(tmpDir, "api.yaml")

		writeFile(t, input, `
openapi: "3.0.0"
paths: {}
`)
		err := OapiYaml(input, filepath.Join(tmpDir, "out.yaml"))
		if err == nil || !strings.Contains(err.Error(), "info") {
			t.Errorf("expected info error, got: %v", err)
		}
	})
}

func TestResolveRef(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		current  string
		expected string
	}{
		{"empty", "", "api.yaml", ""},
		{"relative", "./schemas/user.yaml", "api.yaml", "schemas/user.yaml"},
		{"parent dir", "../common.yaml", "sub/api.yaml", "common.yaml"},
		{"absolute", "/abs/path.yaml", "api.yaml", "/abs/path.yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveRef(tt.path, tt.current)
			if !strings.HasSuffix(got, tt.expected) && got != tt.expected {
				t.Errorf("resolveRef(%q, %q) = %q, want suffix %q", tt.path, tt.current, got, tt.expected)
			}
		})
	}
}

func TestMergeComponents(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	paths := filepath.Join(tmpDir, "paths.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, paths, `
users:
  get:
    responses:
      "200":
        $ref: './responses.yaml#/responses/OK'

components:
  schemas:
    User:
      type: object
`)
	writeFile(t, filepath.Join(tmpDir, "responses.yaml"), `
responses:
  OK:
    description: Success
`)
	writeFile(t, input, `
openapi: "3.0.0"
info:
  title: Test
  version: "1.0"
paths:
  /users:
    $ref: './paths.yaml#/users'
`)
	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(output)
	content := string(data)

	if !strings.Contains(content, "schemas:") {
		t.Error("schemas not merged")
	}
	if !strings.Contains(content, "User:") {
		t.Error("User schema not found")
	}
}

func TestOapiYamlMergesTransitivelyReferencedComponents(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")
	pathsDir := filepath.Join(tmpDir, "paths")
	componentsDir := filepath.Join(tmpDir, "components")
	schemasDir := filepath.Join(componentsDir, "schemas")

	for _, dir := range []string{pathsDir, schemasDir} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
	}

	writeFile(t, input, `
openapi: "3.0.0"
info:
  title: Test
  version: "1.0"
paths:
  /users:
    $ref: './paths/users.yaml#/users'
`)
	writeFile(t, filepath.Join(pathsDir, "users.yaml"), `
users:
  get:
    responses:
      "200":
        $ref: '../components/responses.yaml#/components/responses/OK'
`)
	writeFile(t, filepath.Join(componentsDir, "responses.yaml"), `
components:
  responses:
    OK:
      description: Success
      content:
        application/json:
          schema:
            $ref: './schemas/user.yaml#/components/schemas/User'
`)
	writeFile(t, filepath.Join(schemasDir, "user.yaml"), `
components:
  schemas:
    User:
      type: object
`)

	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read merged output: %v", err)
	}

	var doc yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.UseOrderedMap()); err != nil {
		t.Fatalf("parse merged output: %v", err)
	}
	paths, components := docPathsAndComponents(t, doc)

	pathResponseRef := mapValueAt(
		t,
		paths,
		"/users",
		"get",
		"responses",
		"200",
		"$ref",
	)
	if pathResponseRef != "#/components/responses/OK" {
		t.Errorf("path response ref = %v, want #/components/responses/OK", pathResponseRef)
	}

	responseSchemaRef := mapValueAt(
		t,
		components,
		"responses",
		"OK",
		"content",
		"application/json",
		"schema",
		"$ref",
	)
	if responseSchemaRef != "#/components/schemas/User" {
		t.Errorf("response schema ref = %v, want #/components/schemas/User", responseSchemaRef)
	}

	if got := mapValueAt(
		t,
		components,
		"schemas",
		"User",
		"type",
	); got != "object" {
		t.Errorf("transitively referenced schema type = %v, want object", got)
	}
}

func TestOapiYamlIgnoresReferencesFromDiscardedComponents(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: "3.0.0"
info:
  title: Test
  version: "1.0"
paths:
  /users:
    $ref: './paths.yaml#/users'
components:
  schemas:
    User:
      type: object
`)
	writeFile(t, filepath.Join(tmpDir, "paths.yaml"), `
users:
  get:
    responses:
      "200":
        description: Success
components:
  schemas:
    User:
      $ref: './missing.yaml#/components/schemas/Missing'
`)

	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error from discarded component reference: %v", err)
	}

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read merged output: %v", err)
	}

	var doc yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.UseOrderedMap()); err != nil {
		t.Fatalf("parse merged output: %v", err)
	}
	_, components := docPathsAndComponents(t, doc)

	if got := mapValueAt(
		t,
		components,
		"schemas",
		"User",
		"type",
	); got != "object" {
		t.Errorf("main component type = %v, want object", got)
	}
}

func TestOapiYamlHandlesSymlinkedComponentCycle(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")
	componentsDir := filepath.Join(tmpDir, "components")

	if err := os.MkdirAll(componentsDir, 0750); err != nil {
		t.Fatalf("create components directory: %v", err)
	}
	if err := os.Symlink(".", filepath.Join(componentsDir, "alias")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	writeFile(t, input, `
openapi: "3.0.0"
info:
  title: Test
  version: "1.0"
paths:
  /test:
    $ref: './paths.yaml#/test'
`)
	writeFile(t, filepath.Join(tmpDir, "paths.yaml"), `
test:
  get:
    responses:
      "200":
        description: Success
        content:
          application/json:
            schema:
              $ref: './components/a.yaml#/components/schemas/A'
`)
	writeFile(t, filepath.Join(componentsDir, "a.yaml"), `
components:
  schemas:
    A:
      type: object
      properties:
        b:
          $ref: './alias/b.yaml#/components/schemas/B'
`)
	writeFile(t, filepath.Join(componentsDir, "b.yaml"), `
components:
  schemas:
    B:
      type: object
      properties:
        a:
          $ref: './alias/a.yaml#/components/schemas/A'
`)

	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error from symlinked component cycle: %v", err)
	}

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read merged output: %v", err)
	}

	var doc yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.UseOrderedMap()); err != nil {
		t.Fatalf("parse merged output: %v", err)
	}
	_, components := docPathsAndComponents(t, doc)

	aRef := mapValueAt(
		t,
		components,
		"schemas",
		"A",
		"properties",
		"b",
		"$ref",
	)
	if aRef != "#/components/schemas/B" {
		t.Errorf("A.b ref = %v, want #/components/schemas/B", aRef)
	}

	bRef := mapValueAt(
		t,
		components,
		"schemas",
		"B",
		"properties",
		"a",
		"$ref",
	)
	if bRef != "#/components/schemas/A" {
		t.Errorf("B.a ref = %v, want #/components/schemas/A", bRef)
	}
}

func TestOutputStructure(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: "3.0.0"
tags:
  - name: test
security:
  - bearerAuth: []
info:
  title: Test
  version: "1.0"
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
`)
	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(output)
	content := string(data)

	idxOpenapi := strings.Index(content, "openapi:")
	idxInfo := strings.Index(content, "info:")
	idxPaths := strings.Index(content, "paths:")
	idxComponents := strings.Index(content, "components:")
	idxSecurity := strings.Index(content, "security:")
	idxTags := strings.Index(content, "tags:")

	if idxOpenapi > idxInfo {
		t.Error("openapi should come before info")
	}
	if idxInfo > idxPaths {
		t.Error("info should come before paths")
	}
	if idxPaths > idxComponents {
		t.Error("paths should come before components")
	}
	if idxComponents > idxSecurity {
		t.Error("components should come before security")
	}
	if idxSecurity > idxTags {
		t.Error("security should come before tags")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

// --- OpenAPI 3.2 support ---

func TestOapiYamlOpenAPI32Version(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: 3.2.0
info:
  title: Test
  version: "1.0"
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
`)
	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)
	version, _ := getMapSliceValue(doc, "openapi").(string)
	if version != "3.2.0" {
		t.Errorf("openapi version = %q, want %q", version, "3.2.0")
	}
}

func TestOapiYamlVersionCompatibility(t *testing.T) {
	versions := []string{"3.0.0", "3.0.3", "3.1.0", "3.1.1", "3.2.0", "3.2.5"}

	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			tmpDir := t.TempDir()
			input := filepath.Join(tmpDir, "api.yaml")
			output := filepath.Join(tmpDir, "out.yaml")

			writeFile(t, input, `
openapi: "`+version+`"
info:
  title: Test
  version: "1.0"
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
`)
			if err := OapiYaml(input, output); err != nil {
				t.Fatalf("unexpected error for version %s: %v", version, err)
			}

			doc := unmarshalDoc(t, output)
			got, _ := getMapSliceValue(doc, "openapi").(string)
			if got != version {
				t.Errorf("openapi version = %q, want %q", got, version)
			}
		})
	}
}

func TestOapiYamlHierarchicalTags(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: 3.2.0
info:
  title: Test
  version: "1.0"
tags:
  - name: Catalog

  - name: Categories
    description: Category management
    parent: Catalog

  - name: Items
    parent: Catalog

  - name: Bundles
    parent: Catalog

  - name: FeaturedItems
    parent: Items

  - name: Legacy
    description: Deprecated endpoints
    externalDocs:
      description: Migration guide
      url: https://example.com/migration
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
`)
	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)
	tags := tagsByName(t, doc)

	if len(tags) != 6 {
		t.Fatalf("expected 6 tags, got %d", len(tags))
	}

	// Root tag: no parent.
	if v := getMapSliceValue(tags["Catalog"], "parent"); v != nil {
		t.Errorf("Catalog should not have a parent, got %v", v)
	}

	// Child tag with description.
	if got := getMapSliceValue(tags["Categories"], "parent"); got != "Catalog" {
		t.Errorf("Categories.parent = %v, want Catalog", got)
	}
	if got := getMapSliceValue(tags["Categories"], "description"); got != "Category management" {
		t.Errorf("Categories.description = %v, want %q", got, "Category management")
	}

	// Multiple children of the same parent.
	if got := getMapSliceValue(tags["Items"], "parent"); got != "Catalog" {
		t.Errorf("Items.parent = %v, want Catalog", got)
	}
	if got := getMapSliceValue(tags["Bundles"], "parent"); got != "Catalog" {
		t.Errorf("Bundles.parent = %v, want Catalog", got)
	}

	// Multiple nesting levels: FeaturedItems -> Items -> Catalog.
	if got := getMapSliceValue(tags["FeaturedItems"], "parent"); got != "Items" {
		t.Errorf("FeaturedItems.parent = %v, want Items", got)
	}

	// Tag with externalDocs and no parent.
	if v := getMapSliceValue(tags["Legacy"], "parent"); v != nil {
		t.Errorf("Legacy should not have a parent, got %v", v)
	}
	externalDocs, ok := getMapSliceValue(tags["Legacy"], "externalDocs").(yaml.MapSlice)
	if !ok {
		t.Fatalf("Legacy.externalDocs missing or wrong type")
	}
	if got := getMapSliceValue(externalDocs, "url"); got != "https://example.com/migration" {
		t.Errorf("Legacy.externalDocs.url = %v, want %q", got, "https://example.com/migration")
	}
}

func TestOapiYamlMixed30And32Tags(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: 3.2.0
info:
  title: Test
  version: "1.0"
tags:
  - name: PlainTag
    description: A plain OpenAPI 3.0-style tag with no hierarchy
  - name: Catalog
  - name: Categories
    parent: Catalog
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
`)
	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)
	tags := tagsByName(t, doc)

	if v := getMapSliceValue(tags["PlainTag"], "parent"); v != nil {
		t.Errorf("PlainTag should not have a parent, got %v", v)
	}
	if got := getMapSliceValue(tags["Categories"], "parent"); got != "Catalog" {
		t.Errorf("Categories.parent = %v, want Catalog", got)
	}
}

func TestOapiYamlMultiFileHierarchicalTags(t *testing.T) {
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "root.yaml")
	categories := filepath.Join(tmpDir, "categories.yaml")
	items := filepath.Join(tmpDir, "items.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, root, `
openapi: 3.2.0
info:
  title: Catalog API
  version: "1.0"
tags:
  - name: Catalog
  - name: Categories
    parent: Catalog
  - name: Items
    parent: Catalog
paths:
  /categories:
    $ref: './categories.yaml#/categories'
  /items:
    $ref: './items.yaml#/items'
`)
	writeFile(t, categories, `
categories:
  get:
    tags:
      - Categories
    responses:
      "200":
        description: OK
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/Category'

components:
  schemas:
    Category:
      type: object
      properties:
        name:
          type: string
`)
	writeFile(t, items, `
items:
  get:
    tags:
      - Items
    responses:
      "200":
        description: OK
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/Item'

components:
  schemas:
    Item:
      type: object
      properties:
        name:
          type: string
`)

	if err := OapiYaml(root, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)

	version, _ := getMapSliceValue(doc, "openapi").(string)
	if version != "3.2.0" {
		t.Errorf("openapi version = %q, want 3.2.0", version)
	}

	tags := tagsByName(t, doc)
	if got := getMapSliceValue(tags["Categories"], "parent"); got != "Catalog" {
		t.Errorf("Categories.parent = %v, want Catalog", got)
	}
	if got := getMapSliceValue(tags["Items"], "parent"); got != "Catalog" {
		t.Errorf("Items.parent = %v, want Catalog", got)
	}

	paths, components := docPathsAndComponents(t, doc)
	if got := mapValueAt(t, paths, "/categories", "get", "responses", "200", "content", "application/json", "schema", "$ref"); got != "#/components/schemas/Category" {
		t.Errorf("categories schema ref = %v, want #/components/schemas/Category", got)
	}
	if got := mapValueAt(t, components, "schemas", "Category", "type"); got != "object" {
		t.Errorf("Category.type = %v, want object", got)
	}
	if got := mapValueAt(t, components, "schemas", "Item", "type"); got != "object" {
		t.Errorf("Item.type = %v, want object", got)
	}
}

func TestOapiYamlPreservesUnknownTopLevelFields(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: 3.2.0
summary: Root level summary
info:
  title: Test
  version: "1.0"
jsonSchemaDialect: https://json-schema.org/draft/2020-12/schema
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
webhooks:
  newItem:
    post:
      summary: New item webhook
      responses:
        "200":
          description: OK
x-custom-extension: hello
`)
	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)

	if got := getMapSliceValue(doc, "summary"); got != "Root level summary" {
		t.Errorf("summary = %v, want %q", got, "Root level summary")
	}
	if got := getMapSliceValue(doc, "jsonSchemaDialect"); got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("jsonSchemaDialect = %v", got)
	}
	if got := getMapSliceValue(doc, "x-custom-extension"); got != "hello" {
		t.Errorf("x-custom-extension = %v, want %q", got, "hello")
	}
	webhooks, ok := getMapSliceValue(doc, "webhooks").(yaml.MapSlice)
	if !ok {
		t.Fatalf("webhooks missing or wrong type")
	}
	if got := mapValueAt(t, webhooks, "newItem", "post", "summary"); got != "New item webhook" {
		t.Errorf("webhooks.newItem.post.summary = %v", got)
	}

	// Each unknown field must appear exactly once in the output.
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if n := strings.Count(string(data), "x-custom-extension:"); n != 1 {
		t.Errorf("x-custom-extension appears %d times, want 1", n)
	}
}

func TestOapiYamlResolvesWebhookReferences(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	webhookDefs := filepath.Join(tmpDir, "webhook-defs.yaml")
	schemas := filepath.Join(tmpDir, "schemas.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: 3.1.0
info:
  title: Test
  version: "1.0"
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
webhooks:
  newItem:
    $ref: './webhook-defs.yaml#/newItem'
`)
	writeFile(t, webhookDefs, `
newItem:
  post:
    summary: New item webhook
    requestBody:
      content:
        application/json:
          schema:
            $ref: './schemas.yaml#/components/schemas/Item'
    responses:
      "200":
        description: OK
`)
	writeFile(t, schemas, `
components:
  schemas:
    Item:
      type: object
      properties:
        name:
          type: string
`)

	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)
	webhooks, ok := getMapSliceValue(doc, "webhooks").(yaml.MapSlice)
	if !ok {
		t.Fatalf("webhooks missing or wrong type")
	}

	// The external $ref must be resolved (inlined) rather than left dangling.
	if got := mapValueAt(t, webhooks, "newItem", "post", "summary"); got != "New item webhook" {
		t.Errorf("webhooks.newItem.post.summary = %v, want %q", got, "New item webhook")
	}

	// The transitively referenced schema must be rewritten to a local ref
	// and its content merged into components, exactly like a path $ref.
	schemaRef := mapValueAt(t, webhooks, "newItem", "post", "requestBody", "content", "application/json", "schema", "$ref")
	if schemaRef != "#/components/schemas/Item" {
		t.Errorf("webhook schema ref = %v, want #/components/schemas/Item", schemaRef)
	}
	_, components := docPathsAndComponents(t, doc)
	if got := mapValueAt(t, components, "schemas", "Item", "type"); got != "object" {
		t.Errorf("Item.type = %v, want object", got)
	}
}

func TestOapiYamlResolvesNestedRefsInInlinePaths(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	schemas := filepath.Join(tmpDir, "schemas.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: "3.0.0"
info:
  title: Test
  version: "1.0"
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema:
                $ref: './schemas.yaml#/components/schemas/Item'
`)
	writeFile(t, schemas, `
components:
  schemas:
    Item:
      type: object
      properties:
        name:
          type: string
`)

	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)
	paths, components := docPathsAndComponents(t, doc)

	// The nested external $ref must be rewritten to a local ref, not left
	// pointing at the other file.
	schemaRef := mapValueAt(t, paths, "/test", "get", "responses", "200", "content", "application/json", "schema", "$ref")
	if schemaRef != "#/components/schemas/Item" {
		t.Errorf("inline path schema ref = %v, want #/components/schemas/Item", schemaRef)
	}
	if got := mapValueAt(t, components, "schemas", "Item", "type"); got != "object" {
		t.Errorf("Item.type = %v, want object", got)
	}
}

func TestOapiYamlResolvesNestedRefsInInlineWebhooks(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	schemas := filepath.Join(tmpDir, "schemas.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: 3.1.0
info:
  title: Test
  version: "1.0"
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
webhooks:
  newItem:
    post:
      summary: New item webhook
      requestBody:
        content:
          application/json:
            schema:
              $ref: './schemas.yaml#/components/schemas/Item'
      responses:
        "200":
          description: OK
`)
	writeFile(t, schemas, `
components:
  schemas:
    Item:
      type: object
      properties:
        name:
          type: string
`)

	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)
	webhooks, ok := getMapSliceValue(doc, "webhooks").(yaml.MapSlice)
	if !ok {
		t.Fatalf("webhooks missing or wrong type")
	}

	schemaRef := mapValueAt(t, webhooks, "newItem", "post", "requestBody", "content", "application/json", "schema", "$ref")
	if schemaRef != "#/components/schemas/Item" {
		t.Errorf("inline webhook schema ref = %v, want #/components/schemas/Item", schemaRef)
	}
	_, components := docPathsAndComponents(t, doc)
	if got := mapValueAt(t, components, "schemas", "Item", "type"); got != "object" {
		t.Errorf("Item.type = %v, want object", got)
	}
}

func TestOapiYamlOmitsAbsentWebhooks(t *testing.T) {
	tmpDir := t.TempDir()
	input := filepath.Join(tmpDir, "api.yaml")
	output := filepath.Join(tmpDir, "out.yaml")

	writeFile(t, input, `
openapi: 3.0.0
info:
  title: Test
  version: "1.0"
paths:
  /test:
    get:
      responses:
        "200":
          description: OK
`)
	if err := OapiYaml(input, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	doc := unmarshalDoc(t, output)
	if getMapSliceValue(doc, "webhooks") != nil {
		t.Error("webhooks should not be synthesized when absent from input")
	}
}

func unmarshalDoc(t *testing.T, path string) yaml.MapSlice {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var doc yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.UseOrderedMap()); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	return doc
}

func tagsByName(t *testing.T, doc yaml.MapSlice) map[string]yaml.MapSlice {
	t.Helper()
	rawTags, ok := getMapSliceValue(doc, "tags").([]interface{})
	if !ok {
		t.Fatalf("tags missing or wrong type")
	}
	byName := make(map[string]yaml.MapSlice, len(rawTags))
	for _, rt := range rawTags {
		tag, ok := rt.(yaml.MapSlice)
		if !ok {
			t.Fatalf("tag entry has type %T, want yaml.MapSlice", rt)
		}
		name, _ := getMapSliceValue(tag, "name").(string)
		if name == "" {
			t.Fatalf("tag entry missing name: %+v", tag)
		}
		byName[name] = tag
	}
	return byName
}

func docPathsAndComponents(t *testing.T, doc yaml.MapSlice) (yaml.MapSlice, yaml.MapSlice) {
	t.Helper()
	paths, _ := getMapSliceValue(doc, "paths").(yaml.MapSlice)
	components, _ := getMapSliceValue(doc, "components").(yaml.MapSlice)
	return paths, components
}

func mapValueAt(t *testing.T, root yaml.MapSlice, keys ...string) any {
	t.Helper()

	var value any = root
	for _, key := range keys {
		current, ok := value.(yaml.MapSlice)
		if !ok {
			t.Fatalf("value at %q has type %T, want yaml.MapSlice", strings.Join(keys, "."), value)
		}

		value = getMapSliceValue(current, key)
		if value == nil {
			t.Fatalf("key %q not found at %q", key, strings.Join(keys, "."))
		}
	}

	return value
}
