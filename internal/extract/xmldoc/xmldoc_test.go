package xmldoc

import "testing"

const ublInvoiceXML = `<?xml version="1.0" encoding="UTF-8"?>
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
  <cac:InvoiceLine>
    <cbc:ID>1</cbc:ID>
  </cac:InvoiceLine>
  <cac:LegalMonetaryTotal>
    <cbc:LineExtensionAmount>100000.00</cbc:LineExtensionAmount>
    <cbc:PayableAmount>119000.00</cbc:PayableAmount>
  </cac:LegalMonetaryTotal>
</Invoice>`

func TestParseUBLInvoice(t *testing.T) {
	d, err := Parse([]byte(ublInvoiceXML))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if d.Numero != "FE470" {
		t.Errorf("Numero = %q, want FE470", d.Numero)
	}
	if d.Prefijo != "FE" {
		t.Errorf("Prefijo = %q, want FE", d.Prefijo)
	}
	if d.NIT != "900123456" {
		t.Errorf("NIT = %q, want 900123456", d.NIT)
	}
	if d.RazonSocial != "Proveedor Ejemplo SAS" {
		t.Errorf("RazonSocial = %q", d.RazonSocial)
	}
	if d.FechaEmision != "2026-06-20" {
		t.Errorf("FechaEmision = %q", d.FechaEmision)
	}
	if d.ValorTotal != "119000.00" {
		t.Errorf("ValorTotal = %q, want 119000.00", d.ValorTotal)
	}
	if len(d.CUFE) < 90 {
		t.Errorf("CUFE no extraído correctamente: %q", d.CUFE)
	}
	if !d.IsComplete() {
		t.Errorf("se esperaba factura completa, faltan: %v", d.MissingFields())
	}
}

func TestParseAttachedDocument(t *testing.T) {
	ad := `<?xml version="1.0" encoding="UTF-8"?>
<AttachedDocument xmlns="urn:oasis:names:specification:ubl:schema:xsd:AttachedDocument-2"
                  xmlns:cbc="urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2"
                  xmlns:cac="urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2">
  <cbc:ID>1</cbc:ID>
  <cac:Attachment>
    <cac:ExternalReference>
      <cbc:Description><![CDATA[` + ublInvoiceXML + `]]></cbc:Description>
    </cac:ExternalReference>
  </cac:Attachment>
</AttachedDocument>`

	d, err := Parse([]byte(ad))
	if err != nil {
		t.Fatalf("Parse AttachedDocument error: %v", err)
	}
	if d.Numero != "FE470" || d.NIT != "900123456" {
		t.Errorf("no se desempaquetó el Invoice embebido: %+v", d)
	}
	if !d.IsComplete() {
		t.Errorf("AttachedDocument: factura incompleta, faltan: %v", d.MissingFields())
	}
}
