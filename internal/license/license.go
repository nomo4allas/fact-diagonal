// Package license implementa el licenciamiento del agente en dos niveles:
//
//	Nivel 1 — el hostname real del servidor debe coincidir con ALLOWED_HOST.
//	Nivel 2 — LICENSE_KEY debe ser una clave válida derivada, mediante
//	          HMAC-SHA256, de CLIENT_NAME + ":" + ALLOWED_HOST y una semilla
//	          secreta (secretSeed) que solo existe compilada en el binario.
//
// La semilla NUNCA se lee de config.env ni se registra en logs: es la raíz de
// confianza del esquema y solo vive en el ejecutable. Validate devuelve un error
// genérico (sin indicar cuál de los dos niveles falló) para no dar pistas a quien
// intente sortear la protección.
package license

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"

	"github.com/nomo4allas/fact-diagonal/internal/config"
)

// secretSeed es la clave del HMAC. Constante interna compilada en el binario:
// no debe aparecer nunca en config.env ni en los logs. Generada aleatoriamente.
const secretSeed = "41da41f9cedf1f486b106695c351f6d5f067966a455796020a68c698317ebc589758b39f0b76b930f7decf1bad3df547"

// errInvalidLicense es el único error que expone Validate. Es deliberadamente
// genérico: no revela si falló el Nivel 1 (hostname) o el Nivel 2 (clave).
var errInvalidLicense = errors.New("licencia inválida")

// Generate calcula la clave de licencia para un cliente y un hostname dados.
// La usa Acceso Seguro para emitir claves nuevas. El resultado es el HMAC-SHA256
// de "clientName:hostname" con secretSeed, en hexadecimal en minúsculas.
func Generate(clientName, hostname string) string {
	payload := strings.TrimSpace(clientName) + ":" + strings.TrimSpace(hostname)
	mac := hmac.New(sha256.New, []byte(secretSeed))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Validate comprueba los dos niveles de licenciamiento. Devuelve nil solo si
// AMBOS pasan; en cualquier otro caso devuelve errInvalidLicense sin distinguir
// cuál falló. No registra nada ni intenta notificar: es responsabilidad del
// llamador loguear el mensaje acordado y detener el proceso.
func Validate(cfg *config.Config) error {
	// Nivel 1: el hostname real del servidor debe coincidir con ALLOWED_HOST.
	// Comparación case-insensitive: los nombres de host de Windows no distinguen
	// mayúsculas/minúsculas.
	host, err := os.Hostname()
	if err != nil {
		return errInvalidLicense
	}
	if !strings.EqualFold(strings.TrimSpace(host), cfg.AllowedHost) {
		return errInvalidLicense
	}

	// Nivel 2: la clave del config debe coincidir con la derivada de
	// CLIENT_NAME + ALLOWED_HOST. Comparación en tiempo constante (hmac.Equal)
	// para no filtrar información por tiempo.
	expected := Generate(cfg.ClientName, cfg.AllowedHost)
	if !hmac.Equal([]byte(strings.ToLower(cfg.LicenseKey)), []byte(expected)) {
		return errInvalidLicense
	}

	return nil
}
