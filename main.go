// Command fact-diagonal — Módulos 1 y 2.
//
// Módulo 1: se autentica contra Microsoft Graph (OAuth2 Client Credentials) y
// lista los correos NO leídos del buzón configurado.
//
// Módulo 2: por cada correo con adjuntos descarga los ZIP/PDF, descomprime,
// y extrae los campos de la factura (número, prefijo, NIT, razón social, fecha,
// valor total y CUFE) del XML UBL y del PDF mediante la cascada texto-nativo →
// Tesseract → Gemini.
//
// Opera en MODO SIMULACIÓN: solo lectura, sin mover ni modificar nada.
package main

import (
	"context"
	"os"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/auth"
	"github.com/nomo4allas/fact-diagonal/internal/config"
	"github.com/nomo4allas/fact-diagonal/internal/extract/gemini"
	"github.com/nomo4allas/fact-diagonal/internal/graph"
	"github.com/nomo4allas/fact-diagonal/internal/logger"
	"github.com/nomo4allas/fact-diagonal/internal/pipeline"
)

func main() {
	// Logger a consola + archivo en /logs.
	lg, err := logger.New("logs")
	if err != nil {
		// Sin logger no podemos continuar de forma trazable.
		panic(err)
	}
	defer lg.Close()

	lg.Infof("=== fact-diagonal · Módulos 1 y 2 (lectura de buzón + extracción de adjuntos) ===")

	// 1) Cargar y validar configuración.
	cfg, err := config.Load("config.env")
	if err != nil {
		lg.Errorf("configuración inválida: %v", err)
		os.Exit(1)
	}
	lg.Infof("Buzón objetivo: %s", cfg.Mailbox)
	lg.Infof("Modo simulación: %t (solo lectura, no se modifica nada)", cfg.SimulationMode)

	// Contexto con timeout global. El Módulo 2 descarga adjuntos y puede
	// invocar OCR/Gemini, por lo que damos un margen amplio.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 2) Autenticación OAuth2 Client Credentials.
	authCfg := auth.NewConfig(cfg.TenantID, cfg.ClientID, cfg.ClientSecret)
	if _, err := auth.Token(ctx, authCfg); err != nil {
		lg.Errorf("fallo de autenticación: %v", err)
		os.Exit(1)
	}
	lg.Infof("Autenticación con Microsoft Graph: OK")

	// 3) Cliente de Graph con el http.Client autenticado.
	gc := graph.New(auth.HTTPClient(ctx, authCfg))

	// 4) Listar correos no leídos.
	msgs, err := gc.ListUnreadMessages(ctx, cfg.Mailbox)
	if err != nil {
		lg.Errorf("no se pudieron listar los correos no leídos: %v", err)
		os.Exit(1)
	}

	lg.Infof("Correos no leídos encontrados: %d", len(msgs))
	if len(msgs) == 0 {
		lg.Infof("No hay correos no leídos en la bandeja de entrada.")
		return
	}

	// 5) Mostrar por consola: asunto, remitente, fecha y adjuntos.
	for i, m := range msgs {
		subject := m.Subject
		if subject == "" {
			subject = "(sin asunto)"
		}
		adjuntos := "no"
		if m.HasAttachments {
			adjuntos = "sí"
		}
		lg.Infof("----------------------------------------")
		lg.Infof("[%d] Asunto    : %s", i+1, subject)
		lg.Infof("    Remitente : %s", m.SenderName())
		lg.Infof("    Fecha     : %s", m.ReceivedDateTime.Local().Format("2006-01-02 15:04:05"))
		lg.Infof("    Adjuntos  : %s", adjuntos)
	}
	lg.Infof("----------------------------------------")

	// ===================== Módulo 2 =====================
	lg.Infof("=== Módulo 2 · extracción de datos de adjuntos ===")

	withAtt, err := gc.ListUnreadWithAttachments(ctx, cfg.Mailbox)
	if err != nil {
		lg.Errorf("no se pudieron listar los correos con adjuntos: %v", err)
		os.Exit(1)
	}
	lg.Infof("Correos no leídos con adjuntos: %d", len(withAtt))

	if cfg.GeminiAPIKey == "" {
		lg.Infof("Gemini: deshabilitado (GEMINI_API_KEY vacía); la cascada usará solo texto nativo y OCR.")
	} else {
		lg.Infof("Gemini: habilitado.")
	}

	gem := gemini.New(cfg.GeminiAPIKey)
	proc := pipeline.New(gc, gem, lg, cfg.SimulationMode)

	procesadas := 0
	for i, m := range withAtt {
		subject := m.Subject
		if subject == "" {
			subject = "(sin asunto)"
		}
		lg.Infof("========================================")
		lg.Infof("[%d/%d] Procesando: %s — %s", i+1, len(withAtt), subject, m.SenderName())

		results, err := proc.ProcessMessage(ctx, cfg.Mailbox, m)
		if err != nil {
			lg.Errorf("    error procesando el correo: %v", err)
			continue
		}
		procesadas += len(results)

		if cfg.SimulationMode {
			lg.Infof("    (modo simulación: el correo NO se mueve ni se marca como leído)")
		}
	}

	lg.Infof("========================================")
	lg.Infof("Facturas procesadas: %d (de %d correos con adjuntos)", procesadas, len(withAtt))
	lg.Infof("Proceso finalizado (modo simulación, sin cambios en el buzón).")
}
