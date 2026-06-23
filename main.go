// Command fact-diagonal — Módulo 1: conexión y lectura del buzón MS365.
//
// Lee credenciales de config.env, se autentica contra Microsoft Graph
// mediante OAuth2 Client Credentials y lista los correos NO leídos del
// buzón configurado. Opera en MODO SIMULACIÓN: solo lectura, sin mover ni
// modificar nada.
package main

import (
	"context"
	"os"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/auth"
	"github.com/nomo4allas/fact-diagonal/internal/config"
	"github.com/nomo4allas/fact-diagonal/internal/graph"
	"github.com/nomo4allas/fact-diagonal/internal/logger"
)

func main() {
	// Logger a consola + archivo en /logs.
	lg, err := logger.New("logs")
	if err != nil {
		// Sin logger no podemos continuar de forma trazable.
		panic(err)
	}
	defer lg.Close()

	lg.Infof("=== fact-diagonal · Módulo 1 (lectura de buzón MS365) ===")

	// 1) Cargar y validar configuración.
	cfg, err := config.Load("config.env")
	if err != nil {
		lg.Errorf("configuración inválida: %v", err)
		os.Exit(1)
	}
	lg.Infof("Buzón objetivo: %s", cfg.Mailbox)
	lg.Infof("Modo simulación: %t (solo lectura, no se modifica nada)", cfg.SimulationMode)

	// Contexto con timeout global para toda la operación.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	lg.Infof("Proceso finalizado (modo simulación, sin cambios en el buzón).")
}
