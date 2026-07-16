// Package license implementa el licenciamiento del agente mediante una única
// validación:
//
//	LICENSE_KEY debe ser una clave válida derivada, mediante HMAC-SHA256, de
//	CLIENT_NAME y una semilla secreta (secretSeed) que solo existe compilada en
//	el binario.
//
// La semilla NUNCA se lee de config.env ni se registra en logs: es la raíz de
// confianza del esquema y solo vive en el ejecutable. Validate devuelve un error
// genérico para no dar pistas a quien intente sortear la protección.
//
// ALLOWED_HOST puede seguir presente en config.env por compatibilidad, pero se
// ignora por completo: ya no participa en la validación ni en el cálculo del HMAC.
package license

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/nomo4allas/fact-diagonal/internal/config"
)

// secretSeed es la clave del HMAC. Constante interna compilada en el binario:
// no debe aparecer nunca en config.env ni en los logs. Generada aleatoriamente.
const secretSeed = "41da41f9cedf1f486b106695c351f6d5f067966a455796020a68c698317ebc589758b39f0b76b930f7decf1bad3df547"

// errInvalidLicense es el único error que expone Validate. Es deliberadamente
// genérico: no revela por qué falló la validación.
var errInvalidLicense = errors.New("licencia inválida")

// Generate calcula la clave de licencia para un cliente dado. La usa Acceso
// Seguro para emitir claves nuevas. El resultado es el HMAC-SHA256 de clientName
// con secretSeed, en hexadecimal en minúsculas.
func Generate(clientName string) string {
	payload := strings.TrimSpace(clientName)
	mac := hmac.New(sha256.New, []byte(secretSeed))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Validate comprueba la licencia. Devuelve nil solo si LICENSE_KEY coincide con
// la clave derivada de CLIENT_NAME; en cualquier otro caso devuelve
// errInvalidLicense. No registra nada ni intenta notificar: es responsabilidad
// del llamador loguear el mensaje acordado y detener el proceso.
func Validate(cfg *config.Config) error {
	// La clave del config debe coincidir con la derivada de CLIENT_NAME.
	// Comparación en tiempo constante (hmac.Equal) para no filtrar información
	// por tiempo.
	expected := Generate(cfg.ClientName)
	if !hmac.Equal([]byte(strings.ToLower(cfg.LicenseKey)), []byte(expected)) {
		return errInvalidLicense
	}

	return nil
}
