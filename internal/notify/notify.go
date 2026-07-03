// Package notify envía notificaciones de error por correo a soporte usando el
// cliente de Microsoft Graph (Mejora 1).
//
// Reglas:
//   - Solo se envía UN correo por tipo de error y por corrida, aunque el mismo
//     tipo se repita varias veces (dedup por `tipo`).
//   - Respeta SIMULATION_MODE: en simulación registra en el log lo que enviaría,
//     sin llamar a Graph.
//   - El remitente es el buzón del agente (from) y el destinatario es SMTP_TO (to).
package notify

import (
	"context"
	"fmt"
	"time"
)

// Sender es el subconjunto del cliente de Graph que necesita el notificador.
type Sender interface {
	SendMail(ctx context.Context, from, to, subject, body string) error
}

// Logger es el subconjunto del logger que necesita el paquete.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Notifier envía correos de notificación de error, deduplicando por tipo dentro
// de una misma corrida.
type Notifier struct {
	sender     Sender
	from       string // buzón remitente (Mailbox)
	to         string // destinatario (SMTP_TO)
	log        Logger
	simulation bool
	sent       map[string]bool // tipo de error → ya notificado en esta corrida
}

// New construye un Notifier. Si `to` viene vacío, Notify solo registrará una
// advertencia (no hay a quién notificar).
func New(sender Sender, from, to string, log Logger, simulation bool) *Notifier {
	return &Notifier{
		sender:     sender,
		from:       from,
		to:         to,
		log:        log,
		simulation: simulation,
		sent:       make(map[string]bool),
	}
}

// Notify envía (o simula) un correo de notificación para el `tipo` de error
// indicado, con el `detalle` del mensaje. Deduplica por `tipo`: el segundo y
// siguientes del mismo tipo en la corrida se omiten.
//
// El correo lleva:
//   - Asunto: "Error Agente Facturas - <tipo>"
//   - Cuerpo: fecha/hora, tipo de error y mensaje detallado.
func (n *Notifier) Notify(ctx context.Context, tipo, detalle string) {
	if n.sent[tipo] {
		n.log.Infof("    notificación '%s' ya emitida en esta corrida; se omite el duplicado", tipo)
		return
	}
	n.sent[tipo] = true

	asunto := "Error Agente Facturas - " + tipo
	ahora := time.Now().Format("2006-01-02 15:04:05")
	cuerpo := fmt.Sprintf("Fecha/hora: %s\nTipo de error: %s\n\nDetalle:\n%s\n", ahora, tipo, detalle)

	if n.to == "" {
		n.log.Errorf("    no se puede notificar '%s': SMTP_TO no está configurado (detalle: %s)", tipo, detalle)
		return
	}

	if n.simulation {
		n.log.Infof("    [SIMULACIÓN] se enviaría notificación a %s desde %s | Asunto: %q | %s",
			n.to, n.from, asunto, detalle)
		return
	}

	if err := n.sender.SendMail(ctx, n.from, n.to, asunto, cuerpo); err != nil {
		n.log.Errorf("    no se pudo enviar la notificación '%s' a %s: %v", tipo, n.to, err)
		return
	}
	n.log.Infof("    notificación '%s' enviada a %s ✓", tipo, n.to)
}
