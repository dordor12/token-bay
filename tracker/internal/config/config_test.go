package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestConfig_YAMLTags_AllSnakeCase walks every field in Config and its
// embedded section types and asserts the yaml tag is present and uses
// snake_case. Catches typos and forgotten tags.
func TestConfig_YAMLTags_AllSnakeCase(t *testing.T) {
	walkYAMLTags(t, reflect.TypeOf(Config{}), "Config")
}

func walkYAMLTags(t *testing.T, ty reflect.Type, parent string) {
	t.Helper()
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		tag := f.Tag.Get("yaml")
		path := parent + "." + f.Name
		if tag == "" {
			t.Errorf("%s: missing yaml tag", path)
			continue
		}
		assert.Falsef(t, strings.Contains(tag, " "), "%s: yaml tag must not contain spaces", path)
		assert.Equalf(t, strings.ToLower(tag), tag, "%s: yaml tag %q must be snake_case", path, tag)
		if f.Type.Kind() == reflect.Struct {
			walkYAMLTags(t, f.Type, path)
		}
	}
}

func TestConfig_HasAllExpectedSections(t *testing.T) {
	cty := reflect.TypeOf(Config{})
	expected := []string{
		"DataDir", "LogLevel",
		"Server", "Admin", "Ledger",
		"Broker", "Settlement",
		"Federation", "Reputation", "Admission",
		"STUNTURN", "Metrics",
	}
	for _, name := range expected {
		_, ok := cty.FieldByName(name)
		assert.Truef(t, ok, "Config missing field %q", name)
	}
}
