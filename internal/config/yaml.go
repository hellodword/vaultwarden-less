package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

var errUnknownYAMLField = errors.New("unknown YAML field")

func decodeOneYAML(data []byte, destination any) error {
	if len(data) == 0 || !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8 or empty document")
	}
	file, err := parser.ParseBytes(data, 0)
	if err != nil || len(file.Docs) != 1 || file.Docs[0] == nil || file.Docs[0].Body == nil {
		return fmt.Errorf("invalid YAML document")
	}
	visitor := &strictYAMLVisitor{}
	ast.Walk(visitor, file.Docs[0])
	if visitor.err != nil {
		return fmt.Errorf("YAML must use explicit strings")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data), yaml.DisallowUnknownField())
	if err := decoder.Decode(destination); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unknown field") {
			return errUnknownYAMLField
		}
		return err
	}
	return nil
}

type strictYAMLVisitor struct {
	err error
}

func (visitor *strictYAMLVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil || visitor.err != nil {
		return nil
	}
	switch node.Type() {
	case ast.AnchorType, ast.AliasType, ast.MergeKeyType, ast.TagType, ast.DirectiveType,
		ast.NullType, ast.BoolType, ast.FloatType, ast.InfinityType, ast.NanType:
		visitor.err = fmt.Errorf("forbidden YAML node")
		return nil
	case ast.IntegerType:
		visitor.err = fmt.Errorf("implicit scalar conversion is forbidden")
		return nil
	}
	return visitor
}
