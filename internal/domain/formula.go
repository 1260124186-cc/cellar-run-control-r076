package domain

import (
	"math"
	"strings"
	"time"
)

type FormulaState string

const (
	FormulaDraft    FormulaState = "draft"
	FormulaApproved FormulaState = "approved"
	FormulaRetired  FormulaState = "retired"
)

// IngredientRef pins one ingredient to an approved revision of the cellar
// ingredient catalog. A step can reference several ingredients.
type IngredientRef struct {
	IngredientID string `json:"ingredient_id"`
	Version      int    `json:"version"`
}

type FormulaStep struct {
	Sequence     int             `json:"sequence"`
	Label        string          `json:"label"`
	TemperatureC float64         `json:"temperature_c"`
	HoldMinutes  int             `json:"hold_minutes"`
	Ingredients  []IngredientRef `json:"ingredients"`
}

type Formula struct {
	ID                    string        `json:"id"`
	Name                  string        `json:"name"`
	Style                 string        `json:"style"`
	TargetOriginalGravity float64       `json:"target_original_gravity"`
	TargetFinalGravity    float64       `json:"target_final_gravity"`
	MaxFermentationTempC  float64       `json:"max_fermentation_temp_c"`
	MinimumDays           int           `json:"minimum_days"`
	MaximumDays           int           `json:"maximum_days"`
	Steps                 []FormulaStep `json:"steps"`
	State                 FormulaState  `json:"state"`
	Version               int64         `json:"version"`
	CreatedAt             time.Time     `json:"created_at"`
	UpdatedAt             time.Time     `json:"updated_at"`
	ApprovedAt            *time.Time    `json:"approved_at,omitempty"`
	RetiredAt             *time.Time    `json:"retired_at,omitempty"`
}

func (f Formula) Clone() Formula {
	cloned := f
	cloned.Steps = cloneSteps(f.Steps)
	return cloned
}

// cloneSteps deep-copies a step list so ingredient slices are never shared
// between aggregates. Ingredient lists are normalized to non-nil so the
// in-memory form matches the versioned persisted schema.
func cloneSteps(steps []FormulaStep) []FormulaStep {
	if steps == nil {
		return nil
	}
	copied := make([]FormulaStep, len(steps))
	for index, step := range steps {
		copied[index] = step
		copied[index].Ingredients = append([]IngredientRef{}, step.Ingredients...)
	}
	return copied
}

func (f *Formula) Normalize() {
	f.Name = NormalizeName(f.Name)
	f.Style = NormalizeName(f.Style)
}

func (f Formula) Validate() error {
	if f.Name == "" {
		return FieldError(CodeInvalidInput, "name", "is required")
	}
	if len([]rune(f.Name)) > 120 {
		return FieldError(CodeInvalidInput, "name", "must not exceed 120 characters")
	}
	if f.Style == "" {
		return FieldError(CodeInvalidInput, "style", "is required")
	}
	if len([]rune(f.Style)) > 80 {
		return FieldError(CodeInvalidInput, "style", "must not exceed 80 characters")
	}
	if f.TargetOriginalGravity < 1.030 || f.TargetOriginalGravity > 1.120 {
		return FieldError(CodeInvalidInput, "target_original_gravity",
			"must be between 1.030 and 1.120")
	}
	if f.TargetFinalGravity <= 0 || f.TargetOriginalGravity-f.TargetFinalGravity < 0.005 {
		return FieldError(CodeInvalidInput, "target_final_gravity",
			"must be positive and at least 0.005 below target original gravity")
	}
	if f.TargetFinalGravity > f.TargetOriginalGravity {
		return FieldError(CodeInvalidInput, "target_final_gravity",
			"must not exceed target original gravity")
	}
	if f.MaxFermentationTempC < 5 || f.MaxFermentationTempC > 40 {
		return FieldError(CodeInvalidInput, "max_fermentation_temp_c",
			"must be between 5 and 40 Celsius")
	}
	if f.MinimumDays < 1 || f.MinimumDays > 120 {
		return FieldError(CodeInvalidInput, "minimum_days",
			"must be between 1 and 120")
	}
	if f.MaximumDays < f.MinimumDays || f.MaximumDays > 120 {
		return FieldError(CodeInvalidInput, "maximum_days",
			"must be between minimum days and 120")
	}
	if len(f.Steps) > 24 {
		return FieldError(CodeInvalidInput, "steps", "must contain at most 24 entries")
	}
	if f.State == FormulaApproved || f.State == FormulaRetired {
		if err := ValidateFormulaSteps(f.Steps, true); err != nil {
			return err
		}
	}
	return nil
}

func ValidateFormulaSteps(steps []FormulaStep, required bool) error {
	if required && len(steps) == 0 {
		return FieldError(CodeInvalidInput, "steps", "must contain at least one entry")
	}
	if len(steps) > 24 {
		return FieldError(CodeInvalidInput, "steps", "must contain at most 24 entries")
	}
	for index, step := range steps {
		expected := index + 1
		if step.Sequence != expected {
			return FieldError(CodeInvalidInput, "steps",
				"sequence values must be contiguous and begin at 1")
		}
		if strings.TrimSpace(step.Label) == "" {
			return FieldError(CodeInvalidInput, "steps.label", "is required")
		}
		if len([]rune(step.Label)) > 120 {
			return FieldError(CodeInvalidInput, "steps.label",
				"must not exceed 120 characters")
		}
		if step.TemperatureC < 5 || step.TemperatureC > 90 {
			return FieldError(CodeInvalidInput, "steps.temperature_c",
				"must be between 5 and 90 Celsius")
		}
		if step.HoldMinutes < 1 || step.HoldMinutes > 1440 {
			return FieldError(CodeInvalidInput, "steps.hold_minutes",
				"must be between 1 and 1440")
		}
		if err := validateIngredientRefs(step.Ingredients); err != nil {
			return err
		}
	}
	return nil
}

func validateIngredientRefs(ingredients []IngredientRef) error {
	if len(ingredients) > 16 {
		return FieldError(CodeInvalidInput, "steps.ingredients",
			"must contain at most 16 entries")
	}
	for _, ingredient := range ingredients {
		if strings.TrimSpace(ingredient.IngredientID) == "" {
			return FieldError(CodeInvalidInput, "steps.ingredients.ingredient_id",
				"is required")
		}
		if len([]rune(ingredient.IngredientID)) > 80 {
			return FieldError(CodeInvalidInput, "steps.ingredients.ingredient_id",
				"must not exceed 80 characters")
		}
		if ingredient.Version < 1 {
			return FieldError(CodeInvalidInput, "steps.ingredients.version",
				"must be at least 1")
		}
	}
	return nil
}

func (f *Formula) SetSteps(steps []FormulaStep) error {
	if f.State != FormulaDraft {
		return NewError(CodeFormulaState, "only a draft formula can change process steps")
	}
	if err := ValidateFormulaSteps(steps, false); err != nil {
		return err
	}
	copied := cloneSteps(steps)
	for index := range copied {
		if copied[index].Ingredients == nil {
			copied[index].Ingredients = []IngredientRef{}
		}
	}
	f.Steps = copied
	f.UpdatedAt = time.Now().UTC()
	f.Version++
	return nil
}

func (f *Formula) Approve(now time.Time) error {
	if f.State != FormulaDraft {
		return NewError(CodeFormulaState, "only a draft formula can be approved")
	}
	if err := ValidateFormulaSteps(f.Steps, true); err != nil {
		return err
	}
	stamp := now.UTC()
	f.State = FormulaApproved
	f.ApprovedAt = &stamp
	f.UpdatedAt = stamp
	f.Version++
	return nil
}

func (f *Formula) Retire(now time.Time) error {
	if f.State != FormulaApproved {
		return NewError(CodeFormulaState, "only an approved formula can be retired")
	}
	stamp := now.UTC()
	f.State = FormulaRetired
	f.RetiredAt = &stamp
	f.UpdatedAt = stamp
	f.Version++
	return nil
}

func GravityDifference(original, final float64) float64 {
	return math.Abs(original - final)
}
