package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/attachment"
	"github.com/nomo4allas/fact-diagonal/internal/database"
	"github.com/nomo4allas/fact-diagonal/internal/graph"
	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// --- Dobles de prueba -------------------------------------------------------

// noopLogger descarta todos los mensajes de log.
type noopLogger struct{}

func (noopLogger) Infof(string, ...any)  {}
func (noopLogger) Errorf(string, ...any) {}

// fakeLister implementa attachmentLister devolviendo un conjunto fijo de adjuntos.
type fakeLister struct {
	atts []graph.Attachment
	err  error
}

func (f fakeLister) ListAttachments(_ context.Context, _, _ string) ([]graph.Attachment, error) {
	return f.atts, f.err
}

// fakePersister implementa invoicePersister devolviendo un desenlace de BD fijo,
// sin tocar SQL Server.
type fakePersister struct {
	estado database.EstadoBD
	err    error
}

func (f fakePersister) PersistInvoice(_ context.Context, _ invoice.Data, _ time.Time, _ []database.Adjunto) (database.Persistencia, error) {
	return database.Persistencia{Estado: f.estado}, f.err
}

// ublConCUFE es un XML UBL mínimo con CUFE y varios campos, para que la extracción
// produzca datos aprovechables (FilledCount > 0) sin necesidad de PDF.
const ublConCUFE = `<?xml version="1.0" encoding="UTF-8"?>
<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"
         xmlns:cbc="urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2"
         xmlns:cac="urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2">
  <cbc:ID>FE470</cbc:ID>
  <cbc:UUID>a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00112233445566778899aabbccddeeff00</cbc:UUID>
  <cbc:IssueDate>2026-06-20</cbc:IssueDate>
  <cac:AccountingSupplierParty>
    <cac:Party>
      <cac:PartyName><cbc:Name>Proveedor Ejemplo SAS</cbc:Name></cac:PartyName>
      <cac:PartyTaxScheme>
        <cbc:RegistrationName>Proveedor Ejemplo SAS</cbc:RegistrationName>
        <cbc:CompanyID schemeID="9">900123456</cbc:CompanyID>
      </cac:PartyTaxScheme>
    </cac:Party>
  </cac:AccountingSupplierParty>
  <cac:LegalMonetaryTotal>
    <cbc:PayableAmount>119000.00</cbc:PayableAmount>
  </cac:LegalMonetaryTotal>
</Invoice>`

// --- Pruebas ----------------------------------------------------------------

// 1. Bundle sin datos aprovechables (XML ilegible y sin PDF) → Errores.
func TestProcessBundle_SinDatos_Errores(t *testing.T) {
	p := &Processor{log: noopLogger{}} // sin db ni graph: no se alcanzan

	b := attachment.Bundle{
		Origin:  "factura.zip",
		XML:     []byte("esto no es XML válido"),
		XMLName: "factura.xml",
	}

	_, outcome, err := p.processBundle(context.Background(), b, time.Now())
	if outcome != Errores {
		t.Fatalf("outcome = %v; se esperaba Errores", outcome)
	}
	if err == nil {
		t.Error("se esperaba un detalle de error para notificar, pero err == nil")
	}
}

// 2. Correo cuyos adjuntos no son procesables (no ZIP/PDF) → Errores.
func TestProcessMessage_SinAdjuntosProcesables_Errores(t *testing.T) {
	p := &Processor{
		graph: fakeLister{atts: []graph.Attachment{
			{Name: "aviso.txt", ContentType: "text/plain", ContentBytes: "aGVsbG8="},
		}},
		log: noopLogger{},
	}

	rep := p.ProcessMessage(context.Background(), "buzon@x.co", graph.Message{ID: "m1"})
	if rep.Outcome != Errores {
		t.Fatalf("Outcome = %v; se esperaba Errores", rep.Outcome)
	}
	if rep.Err == nil {
		t.Error("se esperaba rep.Err con el detalle para notificar, pero es nil")
	}
	if len(rep.Results) != 0 {
		t.Errorf("no debería haber resultados; se obtuvieron %d", len(rep.Results))
	}
}

// 3. Extracción OK pero el CUFE no se halla en la BD → Pendientes.
func TestProcessBundle_ExtraccionOK_CUFENoHallado_Pendientes(t *testing.T) {
	p := &Processor{
		db:  fakePersister{estado: database.EstadoNoHallado},
		log: noopLogger{},
	}

	b := attachment.Bundle{Origin: "factura.zip", XML: []byte(ublConCUFE), XMLName: "factura.xml"}

	res, outcome, err := p.processBundle(context.Background(), b, time.Now())
	if err != nil {
		t.Fatalf("no se esperaba error técnico: %v", err)
	}
	if outcome != Pendientes {
		t.Fatalf("outcome = %v; se esperaba Pendientes", outcome)
	}
	if res.Final.FilledCount() == 0 {
		t.Error("la extracción del XML debería haber producido datos (FilledCount > 0)")
	}
}

// 4. Extracción OK + persistencia en BD OK → Procesados.
func TestProcessBundle_ExtraccionOK_BDOK_Procesados(t *testing.T) {
	p := &Processor{
		db:  fakePersister{estado: database.EstadoProcesado},
		log: noopLogger{},
	}

	b := attachment.Bundle{Origin: "factura.zip", XML: []byte(ublConCUFE), XMLName: "factura.xml"}

	_, outcome, err := p.processBundle(context.Background(), b, time.Now())
	if err != nil {
		t.Fatalf("no se esperaba error técnico: %v", err)
	}
	if outcome != Procesados {
		t.Fatalf("outcome = %v; se esperaba Procesados", outcome)
	}
}
