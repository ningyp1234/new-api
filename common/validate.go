package common

import (
	"unicode"

	"github.com/go-playground/validator/v10"
)

var Validate *validator.Validate

func init() {
	Validate = validator.New()
	// SECURITY (H-6): require password complexity
	_ = Validate.RegisterValidation("password_complexity", passwordComplexity)
}

// passwordComplexity enforces:
//   - at least one lowercase letter
//   - at least one uppercase letter
//   - at least one digit
//   - at least one symbol (punct or symbol)
//
// Length is enforced separately by the min/max tags.
func passwordComplexity(fl validator.FieldLevel) bool {
	s := fl.Field().String()
	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, c := range s {
		switch {
		case unicode.IsLower(c):
			hasLower = true
		case unicode.IsUpper(c):
			hasUpper = true
		case unicode.IsDigit(c):
			hasDigit = true
		case unicode.IsPunct(c) || unicode.IsSymbol(c):
			hasSymbol = true
		}
	}
	return hasLower && hasUpper && hasDigit && hasSymbol
}
