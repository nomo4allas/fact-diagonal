// Command fact-diagonal — Módulos 1, 2 y 3.
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
// En cada corrida procesa (Mejora 2): (1) los correos no leídos de la bandeja de
// entrada y (2) los correos de la carpeta /Pendientes. Un correo procesado con
// éxito va a /Procesados; si sigue sin CUFE, queda en /Pendientes; ante un fallo
// técnico se notifica a soporte (Mejora 1) y el correo queda donde estaba
// (Mejora 3: ya no existe la carpeta /Errores).
//
// Respeta SIMULATION_MODE: en simulación solo lee/registra lo que haría, sin
// enviar correos de notificación, sin mover mensajes y sin escribir en la BD.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/auth"
	"github.com/nomo4allas/fact-diagonal/internal/config"
	"github.com/nomo4allas/fact-diagonal/internal/database"
	"github.com/nomo4allas/fact-diagonal/internal/extract/gemini"
	"github.com/nomo4allas/fact-diagonal/internal/graph"
	"github.com/nomo4allas/fact-diagonal/internal/logger"
	"github.com/nomo4allas/fact-diagonal/internal/notify"
	"github.com/nomo4allas/fact-diagonal/internal/pipeline"
)

// Tipos de error para las notificaciones a soporte (Mejora 1).
const (
	tipoErrorSQL = "Conexión SQL Server"
	tipoErrorSP  = "Llamada al SP"
)

// workItem es un correo a procesar junto con la carpeta de la que proviene
// ("Inbox" o "Pendientes"), que determina a dónde se mueve tras procesarlo.
type workItem struct {
	msg    graph.Message
	source string
}

func main() {
	// Logger a consola + archivo en /logs.
	lg, err := logger.New("logs")
	if err != nil {
		// Sin logger no podemos continuar de forma trazable.
		panic(err)
	}
	defer lg.Close()

	lg.Infof("=== fact-diagonal · Módulos 1, 2 y 3 (buzón + extracción + SQL Server) ===")

	// 1) Cargar y validar configuración.
	cfg, err := config.Load("config.env")
	if err != nil {
		lg.Errorf("configuración inválida: %v", err)
		os.Exit(1)
	}
	lg.Infof("Buzón objetivo: %s", cfg.Mailbox)
	if cfg.SimulationMode {
		lg.Infof("Modo simulación: true (solo lectura; no envía, no mueve, no escribe)")
	} else {
		lg.Infof("Modo escritura real: true")
	}

	// Contexto con timeout global. El Módulo 2 descarga adjuntos y puede
	// invocar OCR/Gemini, por lo que damos un margen amplio.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// 2) Autenticación OAuth2 Client Credentials. Un fallo aquí es de conexión a
	// M365: no se puede notificar sin conexión, solo se registra en el log.
	authCfg := auth.NewConfig(cfg.TenantID, cfg.ClientID, cfg.ClientSecret)
	if _, err := auth.Token(ctx, authCfg); err != nil {
		lg.Errorf("fallo de autenticación (conexión M365): %v", err)
		os.Exit(1)
	}
	lg.Infof("Autenticación con Microsoft Graph: OK")

	// 3) Cliente de Graph con el http.Client autenticado.
	gc := graph.New(auth.HTTPClient(ctx, authCfg))

	// Notificador de errores a soporte (Mejora 1). Remitente = buzón; destinatario
	// = SMTP_TO. Deduplica por tipo y respeta SIMULATION_MODE.
	notifier := notify.New(gc, cfg.Mailbox, cfg.SMTPTo, lg, cfg.SimulationMode)
	if cfg.SMTPTo == "" {
		lg.Infof("SMTP_TO no configurado: las notificaciones de error solo se registrarán en el log.")
	} else {
		lg.Infof("Notificaciones de error → %s", cfg.SMTPTo)
	}

	// 4) Listar correos no leídos de la bandeja de entrada. Un fallo aquí es de
	// conexión a M365 → solo log local.
	unread, err := gc.ListUnreadMessages(ctx, cfg.Mailbox)
	if err != nil {
		lg.Errorf("no se pudieron listar los correos no leídos (conexión M365): %v", err)
		os.Exit(1)
	}
	lg.Infof("Correos no leídos encontrados: %d", len(unread))

	// 5) Mostrar por consola: asunto, remitente, fecha y adjuntos.
	for i, m := range unread {
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

	// Construimos la lista de trabajo: (1) no leídos de Inbox con adjuntos y
	// (2) correos de /Pendientes con adjuntos (Mejora 2). Filtramos hasAttachments
	// del lado del cliente (Graph rechaza algunos filtros combinados).
	var work []workItem
	for _, m := range unread {
		if m.HasAttachments {
			work = append(work, workItem{msg: m, source: "Inbox"})
		}
	}
	inboxConAdj := len(work)
	lg.Infof("Correos no leídos con adjuntos (Inbox): %d", inboxConAdj)

	// Releer /Pendientes (solo lectura: no la creamos si no existe). En simulación
	// tampoco se crea; si no existe simplemente no hay backlog que reprocesar.
	if pendID, found, err := gc.FindInboxChildFolder(ctx, cfg.Mailbox, "Pendientes"); err != nil {
		lg.Errorf("no se pudo consultar la carpeta /Pendientes (conexión M365): %v", err)
	} else if found {
		pend, err := gc.ListChildFolderMessages(ctx, cfg.Mailbox, pendID)
		if err != nil {
			lg.Errorf("no se pudieron listar los correos de /Pendientes (conexión M365): %v", err)
		} else {
			n := 0
			for _, m := range pend {
				if m.HasAttachments {
					work = append(work, workItem{msg: m, source: "Pendientes"})
					n++
				}
			}
			lg.Infof("Correos en /Pendientes con adjuntos: %d", n)
		}
	} else {
		lg.Infof("Carpeta /Pendientes aún no existe: no hay backlog que reprocesar.")
	}

	// Filtro opcional por remitente (FILTER_FROM): útil para procesar un correo
	// concreto en pruebas controladas.
	if cfg.FilterFrom != "" {
		needle := strings.ToLower(cfg.FilterFrom)
		var filtered []workItem
		for _, it := range work {
			if strings.Contains(strings.ToLower(it.msg.SenderName()), needle) {
				filtered = append(filtered, it)
			}
		}
		lg.Infof("Filtro FILTER_FROM=%q activo: %d correo(s) coinciden.", cfg.FilterFrom, len(filtered))
		work = filtered
	}

	// Límite de seguridad: procesar como máximo este número de correos por corrida
	// (configurable con MAX_CORREOS; por defecto 5).
	if len(work) > cfg.MaxCorreos {
		lg.Infof("Limitando a los primeros %d correos (de %d) para esta corrida.", cfg.MaxCorreos, len(work))
		work = work[:cfg.MaxCorreos]
	}

	if len(work) == 0 {
		lg.Infof("No hay correos que procesar (bandeja de entrada ni /Pendientes).")
		return
	}

	if cfg.GeminiAPIKey == "" {
		lg.Infof("Gemini: deshabilitado (GEMINI_API_KEY vacía); la cascada usará solo texto nativo y OCR.")
	} else {
		lg.Infof("Gemini: habilitado.")
	}

	// ===================== Módulo 3 =====================
	// Cliente de SQL Server. Si la BD está configurada pero la conexión falla, es
	// un error crítico: se notifica a soporte y se detiene el proceso (Mejora 1).
	var db *database.Client
	if cfg.DBEnabled() {
		dbCfg := database.Config{
			Server:   cfg.DBServer,
			Port:     cfg.DBPort,
			User:     cfg.DBUser,
			Password: cfg.DBPassword,
			NameDMS:  cfg.DBNameDMS,
			NameAdj:  cfg.DBNameAdj,
			SPName:   cfg.SPName,
		}
		var err error
		db, err = database.Open(ctx, dbCfg, lg, cfg.SimulationMode)
		if err != nil {
			lg.Errorf("no se pudo conectar a SQL Server (%s): %v", cfg.DBServer, err)
			notifier.Notify(ctx, tipoErrorSQL,
				fmt.Sprintf("No se pudo conectar a SQL Server (%s): %v", cfg.DBServer, err))
			lg.Errorf("Proceso detenido por fallo de conexión a SQL Server.")
			os.Exit(1)
		}
		defer db.Close()
		lg.Infof("SQL Server: conectado (DMS=%s, Adjuntos=%s). Escrituras %s.",
			cfg.DBNameDMS, cfg.DBNameAdj,
			map[bool]string{true: "SIMULADAS (no se ejecutan)", false: "REALES"}[cfg.SimulationMode])
	} else {
		lg.Infof("Módulo 3 deshabilitado: faltan credenciales de BD en config.env.")
	}

	gem := gemini.New(cfg.GeminiAPIKey)
	proc := pipeline.New(gc, gem, db, lg, cfg.SimulationMode)

	// Resolvemos (creándolas si faltan) las carpetas destino /Procesados y
	// /Pendientes. Solo fuera de simulación: en SIMULATION_MODE no se crea ni se
	// mueve nada, solo se registra el destino en el log. Mejora 3: sin /Errores.
	var carpetas map[string]string // nombre → folderID
	if !cfg.SimulationMode {
		carpetas = make(map[string]string, 2)
		for _, name := range []string{"Procesados", "Pendientes"} {
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
	for i, it := range work {
		m := it.msg
		subject := m.Subject
		if subject == "" {
			subject = "(sin asunto)"
		}
		lg.Infof("========================================")
		lg.Infof("[%d/%d] Procesando (%s): %s — %s", i+1, len(work), it.source, subject, m.SenderName())

		rep := proc.ProcessMessage(ctx, cfg.Mailbox, m)
		procesadas += len(rep.Results)

		// Fallo técnico: el correo NO se mueve, queda donde estaba (Mejora 3).
		if rep.Outcome == pipeline.ErrorTecnico {
			switch rep.ErrKind {
			case pipeline.KindSP:
				// Fallo en la llamada al SP → notificar a soporte (Mejora 1).
				lg.Errorf("    error técnico en el SP: %v", rep.Err)
				notifier.Notify(ctx, tipoErrorSP, rep.Err.Error())
			default:
				// Fallo de conexión a M365 → solo log local (no se puede notificar).
				lg.Errorf("    error de conexión M365: %v (solo log local)", rep.Err)
			}
			lg.Infof("    el correo queda en /%s sin mover.", it.source)
			continue
		}

		// Desenlace normal: destino según el Outcome.
		destino := rep.Outcome.Folder() // "Procesados" | "Pendientes"

		// Si ya está en la carpeta destino (p.ej. /Pendientes sigue Pendientes),
		// no hay nada que mover.
		if it.source == destino {
			lg.Infof("    el correo permanece en /%s.", destino)
			continue
		}

		if cfg.SimulationMode {
			lg.Infof("    [SIMULACIÓN] el correo se movería de /%s a /%s (no se mueve ni se marca como leído).", it.source, destino)
			continue
		}

		destID, ok := carpetas[destino]
		if !ok {
			lg.Errorf("    no se movió el correo: la carpeta /%s no está disponible.", destino)
			continue
		}
		if err := gc.MoveMessage(ctx, cfg.Mailbox, m.ID, destID); err != nil {
			// Un fallo al mover es de conexión a M365 → solo log; queda donde estaba.
			lg.Errorf("    no se pudo mover el correo a /%s (conexión M365): %v", destino, err)
			continue
		}
		lg.Infof("    correo movido de /%s a /%s ✓", it.source, destino)
	}

	lg.Infof("========================================")
	lg.Infof("Facturas procesadas: %d (de %d correos)", procesadas, len(work))
	if cfg.SimulationMode {
		lg.Infof("Proceso finalizado (modo simulación: sin envíos, sin cambios en el buzón ni en la base de datos).")
	} else {
		lg.Infof("Proceso finalizado.")
	}
}
