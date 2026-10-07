package migrate

import (
	"fmt"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// Migration is one approved, ordered schema upgrade step. Apply transforms
// a snapshot from FromVersion to ToVersion; Verify checks the transformed
// snapshot before its result may be recorded or published.
type Migration struct {
	Name        string
	FromVersion int
	ToVersion   int
	Apply       func(domain.Snapshot) (domain.Snapshot, error)
	Verify      func(domain.Snapshot) error
}

// Approved returns the ordered registry of migrations this binary may run.
// A state file is upgraded by applying every step whose FromVersion chain
// leads from the detected version to CurrentVersion.
func Approved() []Migration {
	return []Migration{
		{
			Name:        "formula-step-ingredient-refs",
			FromVersion: LegacyVersion,
			ToVersion:   CurrentVersion,
			Apply:       applyStepIngredientRefs,
			Verify:      verifyStepIngredientRefs,
		},
	}
}

// plan returns the ordered steps that migrate a state file at version
// `from` to CurrentVersion, or ErrNoMigrationPath when the registry cannot
// reach it.
func plan(from int) ([]Migration, error) {
	if from < LegacyVersion || from > CurrentVersion {
		return nil, fmt.Errorf("%w: cannot migrate version %d to version %d",
			ErrNoMigrationPath, from, CurrentVersion)
	}
	var steps []Migration
	version := from
	for version < CurrentVersion {
		advanced := false
		for _, migration := range Approved() {
			if migration.FromVersion == version && migration.ToVersion > version {
				steps = append(steps, migration)
				version = migration.ToVersion
				advanced = true
				break
			}
		}
		if !advanced {
			return nil, fmt.Errorf("%w: cannot migrate version %d to version %d",
				ErrNoMigrationPath, from, CurrentVersion)
		}
	}
	return steps, nil
}

// applyStepIngredientRefs introduces the per-step ingredient reference list
// required by schema version 2. Legacy steps carried no ingredient data, so
// each step receives an explicit empty list; planners attach references in
// later revisions.
func applyStepIngredientRefs(snapshot domain.Snapshot) (domain.Snapshot, error) {
	migrated := snapshot.Clone()
	for id, formula := range migrated.Formulas {
		for index := range formula.Steps {
			if formula.Steps[index].Ingredients == nil {
				formula.Steps[index].Ingredients = []domain.IngredientRef{}
			}
		}
		migrated.Formulas[id] = formula
	}
	return migrated, nil
}

// verifyStepIngredientRefs checks the migrated snapshot before it may be
// published: every entity must satisfy domain validation, every step must
// carry an explicit ingredient list, and the lookup indexes must still
// match their collections.
func verifyStepIngredientRefs(snapshot domain.Snapshot) error {
	if snapshot.Revision < 0 {
		return fmt.Errorf("snapshot revision cannot be negative")
	}
	for id, formula := range snapshot.Formulas {
		if err := formula.Validate(); err != nil {
			return fmt.Errorf("formula %s: %w", id, err)
		}
		for _, step := range formula.Steps {
			if step.Ingredients == nil {
				return fmt.Errorf("formula %s step %d has no ingredient list",
					id, step.Sequence)
			}
		}
	}
	for id, vessel := range snapshot.Vessels {
		if err := vessel.Validate(); err != nil {
			return fmt.Errorf("vessel %s: %w", id, err)
		}
	}
	for id, run := range snapshot.Runs {
		if err := run.Validate(); err != nil {
			return fmt.Errorf("run %s: %w", id, err)
		}
	}
	if err := snapshot.ValidateConsistency(); err != nil {
		return fmt.Errorf("snapshot indexes: %w", err)
	}
	return nil
}
