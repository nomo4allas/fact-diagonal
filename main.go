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
// Módulo 3: busca cada factura en SQL Server por su CUFE; si existe, actualiza
// los campos de recepción e inserta el PDF y el XML en la tabla Adjuntos.
//
// Opera en MODO SIMULACIÓN: solo lectura/búsqueda, sin escribir en el buzón ni
// en la base de datos.
package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/auth"
	"github.com/nomo4allas/fact-diagonal/internal/config"
	"github.com/nomo4allas/fact-diagonal/internal/database"
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
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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

	// Reutilizamos la lista de no leídos del Módulo 1 y filtramos los que
	// traen adjuntos del lado del cliente. (Graph rechaza el filtro servidor
	// combinado isRead+hasAttachments con "InefficientFilter".)
	var withAtt []graph.Message
	for _, m := range msgs {
		if m.HasAttachments {
			withAtt = append(withAtt, m)
		}
	}
	lg.Infof("Correos no leídos con adjuntos: %d", len(withAtt))

	// Filtro opcional por remitente (FILTER_FROM): útil para procesar un
	// correo concreto en pruebas controladas.
	if cfg.FilterFrom != "" {
		needle := strings.ToLower(cfg.FilterFrom)
		var filtered []graph.Message
		for _, m := range withAtt {
			if strings.Contains(strings.ToLower(m.SenderName()), needle) {
				filtered = append(filtered, m)
			}
		}
		lg.Infof("Filtro FILTER_FROM=%q activo: %d correo(s) coinciden.", cfg.FilterFrom, len(filtered))
		withAtt = filtered
	}

	// Límite de seguridad: procesar como máximo este número de correos por
	// corrida (configurable con MAX_CORREOS; por defecto 5). Útil para pruebas
	// controladas en modo simulación.
	if len(withAtt) > cfg.MaxCorreos {
		lg.Infof("Limitando a los primeros %d correos (de %d) para esta corrida.", cfg.MaxCorreos, len(withAtt))
		withAtt = withAtt[:cfg.MaxCorreos]
	}

	if cfg.GeminiAPIKey == "" {
		lg.Infof("Gemini: deshabilitado (GEMINI_API_KEY vacía); la cascada usará solo texto nativo y OCR.")
	} else {
		lg.Infof("Gemini: habilitado.")
	}

	// ===================== Módulo 3 =====================
	// Cliente de SQL Server (opcional). Si la BD no está configurada o no
	// responde, continuamos sin Módulo 3 (la extracción del Módulo 2 sigue).
	var db *database.Client
	if cfg.DBEnabled() {
		dbCfg := database.Config{
			Server:   cfg.DBServer,
			Port:     cfg.DBPort,
			User:     cfg.DBUser,
			Password: cfg.DBPassword,
			NameDMS:  cfg.DBNameDMS,
			NameAdj:  cfg.DBNameAdj,
		}
		var err error
		db, err = database.Open(ctx, dbCfg, lg, cfg.SimulationMode)
		if err != nil {
			lg.Errorf("Módulo 3 deshabilitado: no se pudo conectar a SQL Server (%s): %v", cfg.DBServer, err)
			db = nil
		} else {
			defer db.Close()
			lg.Infof("SQL Server: conectado (DMS=%s, Adjuntos=%s). Escrituras %s.",
				cfg.DBNameDMS, cfg.DBNameAdj,
				map[bool]string{true: "SIMULADAS (no se ejecutan)", false: "REALES"}[cfg.SimulationMode])
		}
	} else {
		lg.Infof("Módulo 3 deshabilitado: faltan credenciales de BD en config.env.")
	}

	gem := gemini.New(cfg.GeminiAPIKey)
	proc := pipeline.New(gc, gem, db, lg, cfg.SimulationMode)

	// Ajuste "lógica de carpetas": resolvemos (creándolas si faltan) las
	// subcarpetas de Inbox destino. Solo fuera de simulación: en SIMULATION_MODE
	// no se crea ni se mueve nada, solo se registra el destino en el log.
	var carpetas map[string]string // nombre → folderID
	if !cfg.SimulationMode {
		carpetas = make(map[string]string, 3)
		for _, name := range []string{"Procesados", "Pendientes", "Errores"} {
			id, err := gc.ResolveInboxChildFolder(ctx, cfg.Mailbox, name)
			if err != nil {
				lg.Errorf("no se pudo resolver/crear la carpeta /%s: %v", name, err)
				continue
			}
			carpetas[name] = id
			lg.Infof("Carpeta destino /%s lista.", name)
		}
	}

	procesadas := 0
	for i, m := range withAtt {
		subject := m.Subject
		if subject == "" {
			subject = "(sin asunto)"
		}
		lg.Infof("========================================")
		lg.Infof("[%d/%d] Procesando: %s — %s", i+1, len(withAtt), subject, m.SenderName())

		results, outcome, err := proc.ProcessMessage(ctx, cfg.Mailbox, m)
		if err != nil {
			// El desenlace ya viene como Errores; seguimos para clasificarlo.
			lg.Errorf("    error procesando el correo: %v", err)
		}
		procesadas += len(results)

		// Clasificación de carpeta según el desenlace del correo.
		carpeta := outcome.Folder()
		if cfg.SimulationMode {
			lg.Infof("    [SIMULACIÓN] el correo se movería a /%s (no se mueve ni se marca como leído)", carpeta)
			continue
		}
		destID, ok := carpetas[carpeta]
		if !ok {
			lg.Errorf("    no se movió el correo: la carpeta /%s no está disponible", carpeta)
			continue
		}
		if err := gc.MoveMessage(ctx, cfg.Mailbox, m.ID, destID); err != nil {
			lg.Errorf("    no se pudo mover el correo a /%s: %v", carpeta, err)
			continue
		}
		lg.Infof("    correo movido a /%s ✓", carpeta)
	}

	lg.Infof("========================================")
	lg.Infof("Facturas procesadas: %d (de %d correos con adjuntos)", procesadas, len(withAtt))
	if cfg.SimulationMode {
		lg.Infof("Proceso finalizado (modo simulación: sin cambios en el buzón ni en la base de datos).")
	} else {
		lg.Infof("Proceso finalizado.")
	}
}
