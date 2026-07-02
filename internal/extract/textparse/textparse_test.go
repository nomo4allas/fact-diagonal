package textparse

import "testing"

func TestParseTextoFactura(t *testing.T) {
	text := `REPRESENTACION GRAFICA FACTURA ELECTRONICA DE VENTA
Proveedor Ejemplo SAS
NIT: 900.123.456-7
Factura No. FE-470
Fecha de emisión: 2026-06-20
Total a pagar $ 119.000,00
CUFE: a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00112233445566778899aabbccddeeff00`

	d := Parse(text)

	if d.NIT != "900.123.456-7" {
		t.Errorf("NIT = %q", d.NIT)
	}
	if d.Numero != "FE470" {
		t.Errorf("Numero = %q, want FE470", d.Numero)
	}
	if d.Prefijo != "FE" {
		t.Errorf("Prefijo = %q, want FE", d.Prefijo)
	}
	if d.FechaEmision != "2026-06-20" {
		t.Errorf("FechaEmision = %q", d.FechaEmision)
	}
	if d.ValorTotal != "119.000,00" {
		t.Errorf("ValorTotal = %q, want 119.000,00", d.ValorTotal)
	}
	if len(d.CUFE) < 90 {
		t.Errorf("CUFE = %q", d.CUFE)
	}
}

// TestParseCamposAdicionales cubre los campos del ajuste Módulo 2 extraídos del
// PDF: PEDIDO No:, DECLARAC: y el BL (DOCTTE: / N° BL:).
func TestParseCamposAdicionales(t *testing.T) {
	t.Run("DOCTTE como BL", func(t *testing.T) {
		text := `Factura No. FE-470
PEDIDO No: 4500123456
DECLARAC: DEX-2026-778
DOCTTE: MAEU-99887766`
		d := Parse(text)
		if d.Pedido != "4500123456" {
			t.Errorf("Pedido = %q, want 4500123456", d.Pedido)
		}
		if d.Declarac != "DEX-2026-778" {
			t.Errorf("Declarac = %q, want DEX-2026-778", d.Declarac)
		}
		if d.BL != "MAEU-99887766" {
			t.Errorf("BL = %q, want MAEU-99887766", d.BL)
		}
	})

	t.Run("N° BL como BL", func(t *testing.T) {
		text := `PEDIDO 778899
N° BL: HLCU12345`
		d := Parse(text)
		if d.Pedido != "778899" {
			t.Errorf("Pedido = %q, want 778899", d.Pedido)
		}
		if d.BL != "HLCU12345" {
			t.Errorf("BL = %q, want HLCU12345", d.BL)
		}
	})
}

// TestParseFalsosPositivosExtras reproduce los falsos positivos observados en la
// corrida real contra el buzón y verifica que los regex endurecidos ya no los
// capturan (misma línea + \b en BL + palabras reservadas).
func TestParseFalsosPositivosExtras(t *testing.T) {
	t.Run("FECHA no es Pedido (valor en la línea siguiente)", func(t *testing.T) {
		// "PEDIDO No:" al final de la línea; el valor real está en otra celda.
		d := Parse("PEDIDO No:\nFECHA DE VENCIMIENTO 2026-07-01")
		if d.Pedido != "" {
			t.Errorf("Pedido = %q, want vacío (no debe cruzar salto de línea)", d.Pedido)
		}
	})

	t.Run("DECLARAC no es BL (palabra reservada)", func(t *testing.T) {
		d := Parse("N° BL: DECLARAC 92352601746687")
		if d.BL != "" {
			t.Errorf("BL = %q, want vacío (DECLARAC es palabra reservada)", d.BL)
		}
	})

	t.Run("ANQ no es BL (sin token BL real)", func(t *testing.T) {
		d := Parse("Cliente Amable ANQ 5 unidades")
		if d.BL != "" {
			t.Errorf("BL = %q, want vacío (no hay token BL, 'bl' va embebido)", d.BL)
		}
	})

	t.Run("igar no es BL ('bl' embebido en Obligaciones)", func(t *testing.T) {
		d := Parse("Cumplimiento de Obligaciones igar")
		if d.BL != "" {
			t.Errorf("BL = %q, want vacío ('bl' de Obligaciones no es token BL)", d.BL)
		}
	})
}
