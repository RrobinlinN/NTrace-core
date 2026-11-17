// Backbone highlighting for hop geo lines, mirroring the CLI
// (printer/realtime_common.go: formatWhoisPrefix + highlightRealtimeBackbone).
// The CLI prints the ASN and whois prefix of premium backbones (CN2, CMIN2,
// CU 9929/10099, CTG ...) in yellow and every other ASN in green.
(function(root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.nextTraceRouteClass = api;
  }
})(typeof globalThis !== 'undefined' ? globalThis : this, function() {
  const BACKBONE_ASNS = new Set(['58807', '10099', '4809', '9929', '23764']);
  const BACKBONE_WHOIS = new Set(['[CTG-CN]', '[CNC-BACKBONE]', '[CUG-BACKBONE]', '[CMIN2-NET]']);

  // "CMIN2-NET-XX" -> "[CMIN2-NET]". Reserved ranges (RFC1918 ...) are dropped
  // when suppressReserved is set, as the CLI does for IPv4 hops.
  function whoisPrefix(whois, suppressReserved) {
    const parts = String(whois || '').split('-');
    const prefix = parts.length > 1 ? parts.slice(0, 2).join('-') : parts[0];
    if (!prefix) {
      return '';
    }
    if (suppressReserved && (prefix.startsWith('RFC') || prefix.startsWith('DOD'))) {
      return '';
    }
    return '[' + prefix + ']';
  }

  function classify(geo, ip) {
    const asn = geo && geo.asnumber ? String(geo.asnumber) : '';
    const prefix = whoisPrefix(geo && geo.whois, true);
    const backbone = BACKBONE_ASNS.has(asn) || BACKBONE_WHOIS.has(prefix) ||
      String(ip || '').startsWith('59.43.');
    return {asn, whoisPrefix: prefix, backbone};
  }

  // Fills el with "AS1234 [WHOIS] · rest", colouring the ASN and whois prefix.
  // segments is the already localized list whose first entry may be the
  // "AS1234" text; it is replaced by the styled version.
  function fillGeoLine(el, segments, geo, ip) {
    const info = classify(geo, ip);
    const rest = segments.filter((segment) => segment && segment !== 'AS' + info.asn);
    el.textContent = '';
    el.classList.toggle('geo--backbone', info.backbone);

    const lead = [];
    if (info.asn) {
      lead.push('AS' + info.asn);
    }
    if (info.whoisPrefix) {
      lead.push(info.whoisPrefix);
    }
    if (lead.length > 0) {
      const tag = el.ownerDocument.createElement('span');
      tag.className = info.backbone ? 'geo-asn geo-asn--backbone' : 'geo-asn';
      tag.textContent = lead.join(' ');
      el.appendChild(tag);
    }
    if (rest.length > 0) {
      const prefix = lead.length > 0 ? ' · ' : '';
      el.appendChild(el.ownerDocument.createTextNode(prefix + rest.join(' · ')));
    }
    return lead.length > 0 || rest.length > 0;
  }

  return {whoisPrefix, classify, fillGeoLine};
});
