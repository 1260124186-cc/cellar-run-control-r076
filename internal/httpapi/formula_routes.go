package httpapi

import (
	"net/http"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

type createFormulaRequest struct {
	Name                  string  `json:"name"`
	Style                 string  `json:"style"`
	TargetOriginalGravity float64 `json:"target_original_gravity"`
	TargetFinalGravity    float64 `json:"target_final_gravity"`
	MaxFermentationTempC  float64 `json:"max_fermentation_temp_c"`
	MinimumDays           int     `json:"minimum_days"`
	MaximumDays           int     `json:"maximum_days"`
}

type setStepsRequest struct {
	Steps []domain.FormulaStep `json:"steps"`
}

type setIngredientsRequest struct {
	Ingredients []domain.FormulaIngredient `json:"ingredients"`
}

func registerFormulaRoutes(mux *http.ServeMux, app *service.Service) {
	mux.HandleFunc("POST /v1/formulas", func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[createFormulaRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.CreateFormula(service.CreateFormulaInput{
			Name:                  body.Name,
			Style:                 body.Style,
			TargetOriginalGravity: body.TargetOriginalGravity,
			TargetFinalGravity:    body.TargetFinalGravity,
			MaxFermentationTempC:  body.MaxFermentationTempC,
			MinimumDays:           body.MinimumDays,
			MaximumDays:           body.MaximumDays,
		})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusCreated, formula)
	})

	mux.HandleFunc("GET /v1/formulas", func(response http.ResponseWriter, _ *http.Request) {
		formulas, err := app.ListFormulas()
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"formulas": formulas})
	})

	mux.HandleFunc("GET /v1/formulas/{id}", func(response http.ResponseWriter, request *http.Request) {
		formula, err := app.GetFormula(request.PathValue("id"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	})

	mux.HandleFunc("PUT /v1/formulas/{id}/steps", func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[setStepsRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.SetFormulaSteps(request.PathValue("id"),
			service.SetFormulaStepsInput{ExpectedVersion: expected, Steps: body.Steps})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	})

	mux.HandleFunc("PUT /v1/formulas/{id}/ingredients", func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[setIngredientsRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.SetFormulaIngredients(request.PathValue("id"),
			service.SetFormulaIngredientsInput{ExpectedVersion: expected, Ingredients: body.Ingredients})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	})

	mux.HandleFunc("POST /v1/formulas/{id}/approve", func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.ApproveFormula(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	})

	mux.HandleFunc("POST /v1/formulas/{id}/retire", func(response http.ResponseWriter, request *http.Request) {
		expected, err := expectedVersion(request)
		if !handleDecode(response, err) {
			return
		}
		formula, err := app.RetireFormula(request.PathValue("id"),
			service.VersionInput{ExpectedVersion: expected})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, formula)
	})
}
