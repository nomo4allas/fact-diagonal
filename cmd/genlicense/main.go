// Command genlicense emite claves de licencia para el agente fact-diagonal.
//
// Lo usa Acceso Seguro para generar la LICENSE_KEY de un cliente nuevo sin tocar
// el código. Recibe un argumento —CLIENT_NAME— y muestra la línea lista para
// pegar en config.env.
//
// Uso:
//
//	go run ./cmd/genlicense DIAGONAL
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
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Uso: go run ./cmd/genlicense <CLIENT_NAME>\n")
		fmt.Fprintf(os.Stderr, "Ejemplo: go run ./cmd/genlicense DIAGONAL\n")
		os.Exit(1)
	}

	clientName := os.Args[1]

	key := license.Generate(clientName)
	fmt.Printf("LICENSE_KEY=%s\n", key)
}
