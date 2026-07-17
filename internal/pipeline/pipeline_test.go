package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/attachment"
	"github.com/nomo4allas/fact-diagonal/internal/database"
	"github.com/nomo4allas/fact-diagonal/internal/extract/gemini"
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

// 1. Bundle sin CUFE (XML ilegible y sin PDF) → SinFactura, sin error técnico.
func TestProcessBundle_SinCUFE_SinFactura(t *testing.T) {
	p := &Processor{log: noopLogger{}} // sin db ni graph: no se alcanzan

	b := attachment.Bundle{
		Origin:  "factura.zip",
		XML:     []byte("esto no es XML válido"),
		XMLName: "factura.xml",
	}

	_, outcome, err := p.processBundle(context.Background(), b, time.Now())
	if outcome != SinFactura {
		t.Fatalf("outcome = %v; se esperaba SinFactura", outcome)
	}
	if err != nil {
		t.Errorf("SinFactura no es un error técnico; err debería ser nil, fue: %v", err)
	}
}

// 2. Correo cuyos adjuntos no son procesables (no ZIP/PDF) → SinFactura: se
// marcará como leído y se dejará donde está, sin error técnico.
func TestProcessMessage_SinAdjuntosProcesables_SinFactura(t *testing.T) {
	p := &Processor{
		graph: fakeLister{atts: []graph.Attachment{
			{Name: "aviso.txt", ContentType: "text/plain", ContentBytes: "aGVsbG8="},
		}},
		log: noopLogger{},
	}

	rep := p.ProcessMessage(context.Background(), "buzon@x.co", graph.Message{ID: "m1"})
	if rep.Outcome != SinFactura {
		t.Fatalf("Outcome = %v; se esperaba SinFactura", rep.Outcome)
	}
	if rep.Err != nil {
		t.Errorf("SinFactura no es un error técnico; rep.Err debería ser nil, fue: %v", rep.Err)
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

// 5. Mejora 2 — XML completo (FilledCount>=6 con CUFE) y además hay PDF: la
// cascada del PDF se salta por completo (ni siquiera se consulta a Gemini). El
// consolidado sale íntegro del XML y PDFData queda vacío. Usamos gemini==nil a
// propósito: si la cascada se ejecutara, al llegar al eslabón de Gemini haría
// panic por deref de nil; que la prueba no entre en pánico demuestra que la
// cascada se saltó.
func TestProcessBundle_XMLCompleto_SaltaCascadaPDF(t *testing.T) {
	p := &Processor{log: noopLogger{}} // gemini nil, db nil a propósito

	b := attachment.Bundle{
		Origin:  "factura.zip",
		XML:     []byte(ublConCUFE),
		XMLName: "factura.xml",
		PDF:     []byte("%PDF-1.4 contenido no procesable"),
		PDFName: "factura.pdf",
	}

	res, outcome, err := p.processBundle(context.Background(), b, time.Now())
	if err != nil {
		t.Fatalf("no se esperaba error técnico: %v", err)
	}
	if outcome != Procesados {
		t.Fatalf("outcome = %v; se esperaba Procesados", outcome)
	}
	if res.PDFData.FilledCount() != 0 {
		t.Errorf("la cascada del PDF no debió ejecutarse; PDFData debería estar vacío, FilledCount=%d", res.PDFData.FilledCount())
	}
	if res.Final.FilledCount() < 6 || res.Final.CUFE == "" {
		t.Errorf("el consolidado debería venir completo del XML; FilledCount=%d, CUFE=%q", res.Final.FilledCount(), res.Final.CUFE)
	}
}

// 6. Mejora 2 (contraparte) — XML incompleto (sin CUFE) con PDF: la cascada del
// PDF SÍ debe ejecutarse. Con un gemini no disponible (sin API key) la cascada
// corre sus eslabones sin panic y sin extraer nada del PDF ilegible; el bundle
// queda sin CUFE → SinFactura.
func TestProcessBundle_XMLIncompleto_EjecutaCascadaPDF(t *testing.T) {
	p := &Processor{
		gemini: gemini.New("", "", ""), // no disponible: la cascada omite Gemini sin panic
		log:    noopLogger{},
	}

	// XML sin CUFE ni campos aprovechables → fuerza la cascada del PDF.
	b := attachment.Bundle{
		Origin:  "factura.zip",
		XML:     []byte("esto no es XML válido"),
		XMLName: "factura.xml",
		PDF:     []byte("%PDF-1.4 contenido no procesable"),
		PDFName: "factura.pdf",
	}

	res, outcome, err := p.processBundle(context.Background(), b, time.Now())
	if err != nil {
		t.Fatalf("no se esperaba error técnico: %v", err)
	}
	if outcome != SinFactura {
		t.Fatalf("outcome = %v; se esperaba SinFactura (sin CUFE)", outcome)
	}
	if res.Final.CUFE != "" {
		t.Errorf("no debería haber CUFE; se obtuvo %q", res.Final.CUFE)
	}
}
