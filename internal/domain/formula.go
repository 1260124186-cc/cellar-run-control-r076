package domain

import (
	"math"
	"regexp"
	"strings"
	"time"
)

type FormulaState string

const (
	FormulaDraft    FormulaState = "draft"
	FormulaApproved FormulaState = "approved"
	FormulaRetired  FormulaState = "retired"
)

// FormulaStep is one ordered process step. Schema version 2 adds the
// optional template reference pair so a step can point at the versioned
// process template it was derived from.
type FormulaStep struct {
	Sequence        int     `json:"sequence"`
	Label           string  `json:"label"`
	TemperatureC    float64 `json:"temperature_c"`
	HoldMinutes     int     `json:"hold_minutes"`
	TemplateRef     string  `json:"template_ref,omitempty"`
	TemplateVersion int64   `json:"template_version,omitempty"`
}

// FormulaIngredient is one raw material line of a formula. Ingredients
// were introduced by schema version 2; formulas migrated from version 1
// carry an empty list until a planner fills it in.
type FormulaIngredient struct {
	Sequence int     `json:"sequence"`
	Name     string  `json:"name"`
	Amount   float64 `json:"amount"`
	Unit     string  `json:"unit"`
}

type Formula struct {
	ID                    string              `json:"id"`
	Name                  string              `json:"name"`
	Style                 string              `json:"style"`
	TargetOriginalGravity float64             `json:"target_original_gravity"`
	TargetFinalGravity    float64             `json:"target_final_gravity"`
	MaxFermentationTempC  float64             `json:"max_fermentation_temp_c"`
	MinimumDays           int                 `json:"minimum_days"`
	MaximumDays           int                 `json:"maximum_days"`
	Ingredients           []FormulaIngredient `json:"ingredients"`
	Steps                 []FormulaStep       `json:"steps"`
	State                 FormulaState        `json:"state"`
	Version               int64               `json:"version"`
	CreatedAt             time.Time           `json:"created_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
	ApprovedAt            *time.Time          `json:"approved_at,omitempty"`
	RetiredAt             *time.Time          `json:"retired_at,omitempty"`
}

func (f Formula) Clone() Formula {
	cloned := f
	if f.Ingredients != nil {
		cloned.Ingredients = append([]FormulaIngredient{}, f.Ingredients...)
	}
	cloned.Steps = append([]FormulaStep(nil), f.Steps...)
	return cloned
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
	if err := ValidateFormulaIngredients(f.Ingredients); err != nil {
		return err
	}
	if f.State == FormulaApproved || f.State == FormulaRetired {
		if err := ValidateFormulaSteps(f.Steps, true); err != nil {
			return err
		}
	}
	return nil
}

// templateRefPattern keeps version references in a stable, portable shape
// so migrated and planner-supplied references compare cleanly.
var templateRefPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,119}$`)

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
		if err := validateTemplateReference(step); err != nil {
			return err
		}
	}
	return nil
}

func validateTemplateReference(step FormulaStep) error {
	if step.TemplateRef == "" {
		if step.TemplateVersion != 0 {
			return FieldError(CodeInvalidInput, "steps.template_version",
				"requires a template reference")
		}
		return nil
	}
	if !templateRefPattern.MatchString(step.TemplateRef) {
		return FieldError(CodeInvalidInput, "steps.template_ref",
			"must start with a lowercase letter or digit and contain only lowercase letters, digits, dots, underscores, slashes, or hyphens")
	}
	if step.TemplateVersion < 1 {
		return FieldError(CodeInvalidInput, "steps.template_version",
			"must be at least 1 when a template reference is present")
	}
	return nil
}

// ValidateFormulaIngredients checks the raw material list. The list may be
// empty: formulas migrated from schema version 1 carry no ingredients until
// a planner records them.
func ValidateFormulaIngredients(ingredients []FormulaIngredient) error {
	if len(ingredients) > 24 {
		return FieldError(CodeInvalidInput, "ingredients",
			"must contain at most 24 entries")
	}
	for index, ingredient := range ingredients {
		expected := index + 1
		if ingredient.Sequence != expected {
			return FieldError(CodeInvalidInput, "ingredients",
				"sequence values must be contiguous and begin at 1")
		}
		if strings.TrimSpace(ingredient.Name) == "" {
			return FieldError(CodeInvalidInput, "ingredients.name", "is required")
		}
		if len([]rune(ingredient.Name)) > 120 {
			return FieldError(CodeInvalidInput, "ingredients.name",
				"must not exceed 120 characters")
		}
		if !(ingredient.Amount > 0) {
			return FieldError(CodeInvalidInput, "ingredients.amount",
				"must be greater than zero")
		}
		if strings.TrimSpace(ingredient.Unit) == "" {
			return FieldError(CodeInvalidInput, "ingredients.unit", "is required")
		}
		if len([]rune(ingredient.Unit)) > 24 {
			return FieldError(CodeInvalidInput, "ingredients.unit",
				"must not exceed 24 characters")
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
	f.Steps = append([]FormulaStep(nil), steps...)
	f.UpdatedAt = time.Now().UTC()
	f.Version++
	return nil
}

func (f *Formula) SetIngredients(ingredients []FormulaIngredient) error {
	if f.State != FormulaDraft {
		return NewError(CodeFormulaState, "only a draft formula can change ingredients")
	}
	if err := ValidateFormulaIngredients(ingredients); err != nil {
		return err
	}
	f.Ingredients = append([]FormulaIngredient(nil), ingredients...)
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
