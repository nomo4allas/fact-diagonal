package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// capturaLog es un Logger de prueba que acumula los mensajes.
type capturaLog struct{ lines []string }

func (l *capturaLog) Infof(f string, a ...any) {
	l.lines = append(l.lines, "INFO "+fmt.Sprintf(f, a...))
}
func (l *capturaLog) Errorf(f string, a ...any) {
	l.lines = append(l.lines, "ERR "+fmt.Sprintf(f, a...))
}
func (l *capturaLog) joined() string { return strings.Join(l.lines, "\n") }

// TestParametrosBusquedaSoloEnOp0 fija el reparto acordado con el SP: @nit,
// @NumDoc y @Prefijo solo viajan en la Operacion 0 (donde el SP los usa como
// respaldo del CUFE) y van NULL en las Operaciones 1 y 2, aunque spParams los
// traiga poblados.
func TestParametrosBusquedaSoloEnOp0(t *testing.T) {
	base := spParams{NIT: "900123456", NumDoc: "FES15380", Prefijo: "FES"}

	casos := []struct {
		operacion int
		quiereSet bool
	}{
		{opBuscarCUFE, true},
		{opActualizar, false},
		{opInsertarAdjunto, false},
	}
	for _, k := range casos {
		p := base
		p.Operacion = k.operacion
		nit, numDoc, prefijo := p.argsBusqueda()

		for nombre, v := range map[string]any{"@nit": nit, "@NumDoc": numDoc, "@Prefijo": prefijo} {
			if k.quiereSet && v == nil {
				t.Errorf("Operacion %d: %s = NULL, se esperaba un valor", k.operacion, nombre)
			}
			if !k.quiereSet && v != nil {
				t.Errorf("Operacion %d: %s = %v, se esperaba NULL", k.operacion, nombre, v)
			}
		}
	}

	// Dentro de la Operacion 0, un dato ausente viaja NULL (no la cadena vacía).
	vacia := spParams{Operacion: opBuscarCUFE}
	argNIT, argNumDoc, argPrefijo := vacia.argsBusqueda()
	if argNIT != nil || argNumDoc != nil || argPrefijo != nil {
		t.Errorf("Operacion 0 sin datos: (@nit,@NumDoc,@Prefijo) = (%v,%v,%v), se esperaban NULL", argNIT, argNumDoc, argPrefijo)
	}
}

func TestDSN(t *testing.T) {
	cfg := Config{Server: "localhost", Port: "1433", User: "sa", Password: "p@ss w/ord", NameDMS: "DMSDiagonal"}
	got := cfg.dsn(cfg.NameDMS)

	for _, want := range []string{"sqlserver://", "sa:", "@localhost:1433", "database=DMSDiagonal", "encrypt=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN %q no contiene %q", got, want)
		}
	}
}

func TestDSNPuertoPorDefecto(t *testing.T) {
	cfg := Config{Server: "db", User: "sa", NameDMS: "X"}
	if got := cfg.dsn("X"); !strings.Contains(got, "@db:1433") {
		t.Errorf("se esperaba puerto por defecto 1433, DSN=%q", got)
	}
}

// TestPersistSinCUFE verifica el caso de borde de CUFE vacío: sin CUFE no se
// puede invocar la Operacion 0, así que no se llama al SP (db nil no debe usarse)
// y se reporta EstadoNoHallado (correo → Pendientes).
func TestPersistSinCUFE(t *testing.T) {
	log := &capturaLog{}
	c := &Client{log: log, simulation: true} // db nil: no debe usarse

	res, err := c.PersistInvoice(context.Background(), invoice.Data{CUFE: "   "}, time.Now(), nil)
	if err != nil {
		t.Fatalf("se esperaba nil, got %v", err)
	}
	if res.Estado != EstadoNoHallado {
		t.Errorf("Estado = %v, want EstadoNoHallado", res.Estado)
	}
	if !strings.Contains(log.joined(), "sin CUFE") {
		t.Errorf("no se registró el caso 'sin CUFE'; log:\n%s", log.joined())
	}
}

// TestTruncar verifica el recorte por runas a los límites varchar del SP.
func TestTruncar(t *testing.T) {
	casos := []struct {
		in   string
		max  int
		want string
	}{
		{"001", maxMandato, "001"},
		{"1234567", maxMandato, "123456"},
		{"café", 3, "caf"},
		{"", maxNotasAdjunto, ""},
	}
	for _, k := range casos {
		if got := truncar(k.in, k.max); got != k.want {
			t.Errorf("truncar(%q,%d) = %q, want %q", k.in, k.max, got, k.want)
		}
	}
}

// TestAdjuntoYaRegistrado verifica que el mensaje "ya registrado" del SP se
// detecte como éxito idempotente, sin importar mayúsculas/minúsculas.
func TestAdjuntoYaRegistrado(t *testing.T) {
	casos := []struct {
		mensaje string
		want    bool
	}{
		{"Adjunto ya registrado.", true}, // mensaje exacto del SP de producción
		{"El adjunto ya registrado", true},
		{"YA REGISTRADO en el sistema", true},
		{"Documento Ya Registrado previamente", true},
		{"Insertado correctamente", false},
		{"", false},
		{"registrado", false},
	}
	for _, k := range casos {
		if got := adjuntoYaRegistrado(k.mensaje); got != k.want {
			t.Errorf("adjuntoYaRegistrado(%q) = %v, want %v", k.mensaje, got, k.want)
		}
	}
}

// TestAdjuntoPersistido verifica la regla de éxito/fallo de la Operacion 2 con el
// SP de producción actualizado: "ya registrado" es éxito para cualquier valor de
// @Resultado (antes venía con 0, ahora con el IdAdjunto>0); el único fallo real
// es @Resultado=0 sin ese mensaje.
func TestAdjuntoPersistido(t *testing.T) {
	casos := []struct {
		nombre    string
		resultado int
		mensaje   string
		want      bool
	}{
		{"inserción nueva (IdAdjunto>0)", 1234, "Adjunto insertado.", true},
		{"ya registrado con IdAdjunto>0 (SP nuevo)", 1234, "Adjunto ya registrado.", true},
		{"ya registrado con Resultado=0 (SP antiguo)", 0, "Adjunto ya registrado.", true},
		{"fallo real: Resultado=0 sin 'ya registrado'", 0, "No existe el radicado.", false},
		{"fallo real: Resultado=0 y mensaje vacío", 0, "", false},
	}
	for _, k := range casos {
		if got := adjuntoPersistido(k.resultado, k.mensaje); got != k.want {
			t.Errorf("%s: adjuntoPersistido(%d,%q) = %v, want %v", k.nombre, k.resultado, k.mensaje, got, k.want)
		}
	}
}

// TestNormalizeNIT verifica @nit: solo dígitos, sin puntos ni dígito de
// verificación (el que va tras el último guion).
func TestNormalizeNIT(t *testing.T) {
	casos := []struct{ in, want string }{
		{"900.123.456-7", "900123456"}, // con puntos y DV
		{"900123456-7", "900123456"},   // solo DV
		{"900.123.456", "900123456"},   // solo puntos
		{"901234567", "901234567"},     // ya limpio (cbc:CompanyID del XML)
		{"  900.123.456-7  ", "900123456"},
		{"900 123 456 - 7", "900123456"}, // con espacios alrededor del guion
		{"", ""},
		{"   ", ""},
		{"N/A", ""}, // sin dígitos → NULL
	}
	for _, k := range casos {
		if got := normalizeNIT(k.in); got != k.want {
			t.Errorf("normalizeNIT(%q) = %q, want %q", k.in, got, k.want)
		}
	}
}

// facturaFES15380 son los datos reales extraídos de la factura FES-15380 (log
// del 14/07/2026), usados como escenario de referencia del ajuste de la
// Operacion 0.
var facturaFES15380 = invoice.Data{
	CUFE:    "45c52f79f45e2d1abb8b68d1de874b7dc9fc57e9dbaadcf4c128ec5af6774a61030c31020879bbb31285510aeb0ec8c",
	NIT:     "809.010.841-5",
	Prefijo: "FES",
	Numero:  "FES 15380",
}

// TestOp0FacturaFES15380 comprueba el ajuste de la Operacion 0 sobre los datos
// reales de la factura FES-15380, sin tocar el buzón ni la BD: verifica los tres
// valores que produce datosBusqueda y que solo viajan en la Operacion 0.
//
// No valida lo que el SP hace con ellos (eso vive en el SP de Andrés), solo lo
// que este código envía.
func TestOp0FacturaFES15380(t *testing.T) {
	nit, numDoc, prefijo := datosBusqueda(facturaFES15380)

	// 1) Los tres valores que la Operacion 0 enviará.
	casos := []struct{ param, got, want string }{
		{"@nit", nit, "809010841"},       // sin puntos ni dígito de verificación
		{"@NumDoc", numDoc, "FES 15380"}, // tal como llega del extractor
		{"@Prefijo", prefijo, "FES"},     // en mayúsculas
	}
	for _, k := range casos {
		if k.got != k.want {
			t.Errorf("%s = %q, want %q", k.param, k.got, k.want)
		}
	}

	// 2) Los tres viajan con esos valores en la Operacion 0…
	op0 := spParams{Operacion: opBuscarCUFE, Cufe: facturaFES15380.CUFE, NIT: nit, NumDoc: numDoc, Prefijo: prefijo}
	argNIT, argNumDoc, argPrefijo := op0.argsBusqueda()
	esperados := map[string]any{"@nit": "809010841", "@NumDoc": "FES 15380", "@Prefijo": "FES"}
	for param, got := range map[string]any{"@nit": argNIT, "@NumDoc": argNumDoc, "@Prefijo": argPrefijo} {
		if got != esperados[param] {
			t.Errorf("Operacion 0: %s = %#v, want %#v", param, got, esperados[param])
		}
	}

	// 3) …y NULL en las Operaciones 1 y 2, aunque spParams los traiga poblados.
	for _, op := range []int{opActualizar, opInsertarAdjunto} {
		p := op0
		p.Operacion = op
		nulNIT, nulNumDoc, nulPrefijo := p.argsBusqueda()
		if nulNIT != nil || nulNumDoc != nil || nulPrefijo != nil {
			t.Errorf("Operacion %d: (@nit,@NumDoc,@Prefijo) = (%#v,%#v,%#v), se esperaban NULL",
				op, nulNIT, nulNumDoc, nulPrefijo)
		}
	}
}

// TestOp0FacturaFES15380Simulacion recorre el flujo real de PersistInvoice en
// SIMULATION_MODE con los datos de la factura FES-15380: sin BD (db nil no debe
// usarse) y sin tocar el buzón, comprueba que la línea de log de la Operacion 0
// lleva los tres parámetros con sus valores.
func TestOp0FacturaFES15380Simulacion(t *testing.T) {
	log := &capturaLog{}
	c := &Client{log: log, simulation: true, spName: "sp_ManRadicadoFacturas"}

	res, err := c.PersistInvoice(context.Background(), facturaFES15380, time.Now(), nil)
	if err != nil {
		t.Fatalf("PersistInvoice devolvió error: %v", err)
	}
	// Sin adjuntos no hay nada que insertar → Pendientes.
	if res.Estado != EstadoPendiente {
		t.Errorf("Estado = %v, want EstadoPendiente", res.Estado)
	}

	for _, want := range []string{"@nit=809010841", "@NumDoc=FES 15380", "@Prefijo=FES"} {
		if !strings.Contains(log.joined(), want) {
			t.Errorf("el log de la Operacion 0 no contiene %q; log:\n%s", want, log.joined())
		}
	}
}

// TestDatosBusqueda verifica los tres datos de respaldo que la Operacion 0 envía
// junto al CUFE para que el SP pueda buscar por NIT + número + prefijo. Los tres
// son varchar(20) en el SP y @NumDoc viaja tal cual: la separación del prefijo
// la hace el SP.
func TestDatosBusqueda(t *testing.T) {
	casos := []struct {
		nombre                        string
		data                          invoice.Data
		wantNIT, wantNum, wantPrefijo string
	}{
		{
			nombre:  "factura completa: @NumDoc conserva el prefijo",
			data:    invoice.Data{NIT: "900.123.456-7", Prefijo: "FES", Numero: "FES15380"},
			wantNIT: "900123456", wantNum: "FES15380", wantPrefijo: "FES",
		},
		{
			nombre:  "número con espacio: se envía tal como llega",
			data:    invoice.Data{NIT: "901234567", Prefijo: "FES", Numero: "FES 15380"},
			wantNIT: "901234567", wantNum: "FES 15380", wantPrefijo: "FES",
		},
		{
			nombre:  "número sin prefijo",
			data:    invoice.Data{NIT: "900123456", Numero: "15380"},
			wantNIT: "900123456", wantNum: "15380", wantPrefijo: "NULL",
		},
		{
			// En la BD los prefijos están en mayúsculas y la comparación del SP
			// puede ser sensible a ellas; @NumDoc en cambio va tal cual.
			nombre:  "@Prefijo se pasa a mayúsculas, @NumDoc no",
			data:    invoice.Data{NIT: "900123456", Prefijo: "fes", Numero: "fes15380"},
			wantNIT: "900123456", wantNum: "fes15380", wantPrefijo: "FES",
		},
		{
			nombre:  "datos ausentes → los tres NULL",
			data:    invoice.Data{},
			wantNIT: "NULL", wantNum: "NULL", wantPrefijo: "NULL",
		},
		{
			nombre:  "cada dato es independiente: sin NIT los otros dos igual viajan",
			data:    invoice.Data{Prefijo: "FES", Numero: "FES15380"},
			wantNIT: "NULL", wantNum: "FES15380", wantPrefijo: "FES",
		},
		{
			nombre:  "los tres se recortan a varchar(20)",
			data:    invoice.Data{NIT: strings.Repeat("9", 25), Numero: strings.Repeat("N", 25), Prefijo: strings.Repeat("A", 25)},
			wantNIT: strings.Repeat("9", maxNIT), wantNum: strings.Repeat("N", maxNumDoc), wantPrefijo: strings.Repeat("A", maxPrefijo),
		},
	}
	for _, k := range casos {
		nit, numDoc, prefijo := datosBusqueda(k.data)
		if orNULL(nit) != k.wantNIT {
			t.Errorf("%s: @nit = %s, want %s", k.nombre, orNULL(nit), k.wantNIT)
		}
		if orNULL(numDoc) != k.wantNum {
			t.Errorf("%s: @NumDoc = %s, want %s", k.nombre, orNULL(numDoc), k.wantNum)
		}
		if orNULL(prefijo) != k.wantPrefijo {
			t.Errorf("%s: @Prefijo = %s, want %s", k.nombre, orNULL(prefijo), k.wantPrefijo)
		}
	}
}

// TestDatosBusquedaTiposString fija que los tres datos viajan como string
// (varchar(20) en el SP), sin conversión numérica de @NumDoc.
func TestDatosBusquedaTiposString(t *testing.T) {
	p := spParams{Operacion: opBuscarCUFE, NIT: "900123456", NumDoc: "FES15380", Prefijo: "FES"}
	nit, numDoc, prefijo := p.argsBusqueda()

	for nombre, v := range map[string]any{"@nit": nit, "@NumDoc": numDoc, "@Prefijo": prefijo} {
		if _, ok := v.(string); !ok {
			t.Errorf("%s = %T, se esperaba string (varchar(20) en el SP)", nombre, v)
		}
	}
}

// TestNotasParaAdjunto verifica @NotasAdjunto: el número de documento (recortado)
// para cualquier adjunto, o NULL cuando no está disponible.
func TestNotasParaAdjunto(t *testing.T) {
	deref := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	casos := []struct {
		numDocumento, want string
	}{
		{"FE470", "FE470"},
		{"  FE471 ", "FE471"},
		{"", "<nil>"},
		{"   ", "<nil>"},
	}
	for _, k := range casos {
		if got := deref(notasParaAdjunto(k.numDocumento)); got != k.want {
			t.Errorf("notasParaAdjunto(%q) = %q, want %q", k.numDocumento, got, k.want)
		}
	}
}
