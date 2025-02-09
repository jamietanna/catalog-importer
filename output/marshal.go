package output

import (
	"context"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/incident-io/catalog-importer/client"
	"github.com/incident-io/catalog-importer/expr"
	"github.com/incident-io/catalog-importer/source"
	"github.com/pkg/errors"
	"github.com/samber/lo"
)

type CatalogTypeModel struct {
	Name            string
	Description     string
	TypeName        string
	Ranked          bool
	Attributes      []client.CatalogTypeAttributePayloadV2
	SourceAttribute *Attribute // tracks the origin attribute, if an enum model
}

type CatalogEntryModel struct {
	ExternalID      string
	Name            string
	Aliases         []string
	Rank            int32
	AttributeValues map[string]client.CatalogAttributeBindingPayloadV2
}

// MarshalType builds the base catalog type model for the output, and any associated enum
// types for its attributes.
func MarshalType(output *Output) (base *CatalogTypeModel, enumTypes []*CatalogTypeModel) {
	base = &CatalogTypeModel{
		Name:        output.Name,
		Description: output.Description,
		TypeName:    output.TypeName,
		Ranked:      output.Ranked,
		Attributes:  []client.CatalogTypeAttributePayloadV2{},
	}
	for _, attr := range output.Attributes {
		var attrType string

		// When an attribute is an enum you don't need to define the type as we'll load it
		// from the enum definition.
		if attr.Enum != nil {
			attrType = attr.Enum.TypeName
		} else {
			attrType = attr.Type.String
		}

		base.Attributes = append(
			base.Attributes, client.CatalogTypeAttributePayloadV2{
				Id:    lo.ToPtr(attr.ID),
				Name:  attr.Name,
				Type:  attrType,
				Array: attr.Array,
			})

		// The enums we generate should be returned as types too, as we'll need to sync them
		// just as any other.
		if attr.Enum != nil {
			enumTypes = append(enumTypes, &CatalogTypeModel{
				Name:        attr.Enum.Name,
				Description: attr.Enum.Description,
				TypeName:    attr.Enum.TypeName,
				Ranked:      output.Ranked,
				Attributes: []client.CatalogTypeAttributePayloadV2{
					{
						Id:   lo.ToPtr("description"),
						Name: "Description",
						Type: "String",
					},
				},
				SourceAttribute: lo.ToPtr(*attr),
			})
		}
	}

	return
}

// MarshalEntries builds payloads to for the entries of the given output, assuming those
// entries have already been filtered.
//
// The majority of the work comes from compiling and evaluating the CEL expressions that
// marshal the catalog entries from source.
func MarshalEntries(ctx context.Context, output *Output, entries []source.Entry) ([]*CatalogEntryModel, error) {
	nameProgram, err := expr.Compile(output.Source.Name)
	if err != nil {
		return nil, errors.Wrap(err, "source.name")
	}

	externalIDProgram, err := expr.Compile(output.Source.ExternalID)
	if err != nil {
		return nil, errors.Wrap(err, "source.external_id")
	}

	var rankProgram cel.Program
	if output.Source.Rank.Valid {
		var err error
		rankProgram, err = expr.Compile(output.Source.Rank.String)
		if err != nil {
			return nil, errors.Wrap(err, "source.rank")
		}
	}

	aliasPrograms := []cel.Program{}
	for idx, alias := range output.Source.Aliases {
		prg, err := expr.Compile(alias)
		if err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("alias.%d: compiling alias", idx))
		}

		aliasPrograms = append(aliasPrograms, prg)
	}

	var (
		attributeByID     = map[string]*Attribute{}
		attributePrograms = map[string]cel.Program{}
	)
	for idx, attr := range output.Attributes {
		// Use the attribute ID by default if source isn't explicitly provided.
		source := attr.ID
		if attr.Source.Valid {
			source = attr.Source.String
		}

		prg, err := expr.Compile(source)
		if err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("attributes.%d (id = %s): compiling source", idx, attr.ID))
		}

		attributeByID[attr.ID] = attr
		attributePrograms[attr.ID] = prg
	}

	catalogEntryModels := []*CatalogEntryModel{}
	for _, entry := range entries {
		name, err := expr.Eval[string](ctx, nameProgram, entry)
		if err != nil {
			return nil, errors.Wrap(err, "evaluating entry name")
		}

		externalID, err := expr.Eval[string](ctx, externalIDProgram, entry)
		if err != nil {
			return nil, errors.Wrap(err, "evaluating entry external ID")
		}

		var rank int32
		if rankProgram != nil {
			var err error
			rank, err = expr.Eval[int32](ctx, rankProgram, entry)
			if err != nil {
				return nil, errors.Wrap(err, "evaluating entry external ID")
			}
		}

		// Try to parse each alias as either a string or a string array, then concat and
		// dedupe them together.
		aliases := []string{}
		for idx, aliasProgram := range aliasPrograms {
			toAdd := []string{}

			alias, err := expr.Eval[string](ctx, aliasProgram, entry)
			if err != nil {
				aliasArray, arrayErr := expr.Eval[[]string](ctx, aliasProgram, entry)
				if arrayErr != nil {
					return nil, errors.Wrap(err, fmt.Sprintf("aliases.%d: evaluating entry alias", idx))
				}
				toAdd = append(toAdd, aliasArray...)
			} else {
				toAdd = append(toAdd, alias)
			}

			for _, alias := range toAdd {
				if alias != "" {
					aliases = append(aliases, alias)
				}
			}
		}

		// Attribute values are built best effort, as it might not be the case that upstream
		// source entries have these fields, or have fields of the correct type.
		attributeValues := map[string]client.CatalogAttributeBindingPayloadV2{}
	eachAttribute:
		for attributeID, prg := range attributePrograms {
			binding := client.CatalogAttributeBindingPayloadV2{}

			if attributeByID[attributeID].Array {
				valueLiterals, err := expr.Eval[[]any](ctx, prg, entry)
				if err != nil {
					continue eachAttribute
				}

				arrayValue := []client.CatalogAttributeValuePayloadV2{}
				for _, literalAny := range valueLiterals {
					literal, ok := literalAny.(string)
					if !ok {
						continue
					}

					arrayValue = append(arrayValue, client.CatalogAttributeValuePayloadV2{
						Literal: lo.ToPtr(literal),
					})
				}

				binding.ArrayValue = &arrayValue
			} else {
				literal, err := evaluateEntryWithAttributeType(ctx, prg, entry, attributeByID[attributeID])

				if err != nil {
					continue eachAttribute
				}

				binding.Value = &client.CatalogAttributeValuePayloadV2{
					Literal: lo.ToPtr(literal),
				}
			}

			attributeValues[attributeID] = binding
		}

		catalogEntryModels = append(catalogEntryModels, &CatalogEntryModel{
			Name:            name,
			ExternalID:      externalID,
			Rank:            rank,
			Aliases:         aliases,
			AttributeValues: attributeValues,
		})
	}

	return catalogEntryModels, nil
}

func evaluateEntryWithAttributeType(ctx context.Context, prg cel.Program, entry map[string]any, attribute *Attribute) (string, error) {
	var literal string

	// If we have an attribute type of type Bool or Number, we can try to evaluate the program against the scope
	// with the appropriate type.
	// If that fails, we'll fall back to a string literal since we accept passing a boolean or numeric value
	// as a string literal.
	if attribute != nil && attribute.Type.Valid {
		switch attribute.Type.String {
		case "Bool":
			literal, _ = evaluateEntryWithType[bool](ctx, prg, entry)
			if literal != "" {
				return literal, nil
			}
		case "Number":
			// Number accepts float or int, so we'll try to evaluate as a float first.
			literal, _ = evaluateEntryWithType[float64](ctx, prg, entry)
			if literal != "" {
				return literal, nil
			}
			literal, _ = evaluateEntryWithType[int64](ctx, prg, entry)
			if literal != "" {
				return literal, nil
			}
		}
	}

	// If we have an attribute type of type String, or we failed to evaluate the program against the scope
	// with the appropriate type, we'll try to evaluate as a string literal.
	return evaluateEntryWithType[string](ctx, prg, entry)
}

func evaluateEntryWithType[ReturnType any](ctx context.Context, prg cel.Program, entry map[string]any) (string, error) {
	literal, err := expr.Eval[ReturnType](ctx, prg, entry)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%v", literal), nil
}
