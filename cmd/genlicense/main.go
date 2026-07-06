// Command genlicense emite claves de licencia para el agente fact-diagonal.
//
// Lo usa Acceso Seguro para generar la LICENSE_KEY de un cliente/servidor nuevo
// sin tocar el código. Recibe dos argumentos —CLIENT_NAME y ALLOWED_HOST— y
// muestra la línea lista para pegar en config.env.
//
// Uso:
//
//	go run ./cmd/genlicense DIAGONAL DIAGAPP01
//	→ LICENSE_KEY=a3f8b2c9d1e4...
//
// La semilla secreta del HMAC vive compilada en internal/license; este comando
// solo la usa a través de license.Generate y nunca la expone.
package main

import (
	"fmt"
	"os"

	"github.com/nomo4allas/fact-diagonal/internal/license"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "Uso: go run ./cmd/genlicense <CLIENT_NAME> <ALLOWED_HOST>\n")
		fmt.Fprintf(os.Stderr, "Ejemplo: go run ./cmd/genlicense DIAGONAL DIAGAPP01\n")
		os.Exit(1)
	}

	clientName := os.Args[1]
	allowedHost := os.Args[2]

	key := license.Generate(clientName, allowedHost)
	fmt.Printf("LICENSE_KEY=%s\n", key)
}
