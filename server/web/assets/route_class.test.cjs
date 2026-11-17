const test = require('node:test');
const assert = require('node:assert/strict');

const {whoisPrefix, classify} = require('./route_class.js');

test('whois prefix keeps the first two dash-separated parts', () => {
  assert.equal(whoisPrefix('CMIN2-NET-XX', true), '[CMIN2-NET]');
  assert.equal(whoisPrefix('CHINANET-BACKBONE', true), '[CHINANET-BACKBONE]');
  assert.equal(whoisPrefix('APNIC', true), '[APNIC]');
  assert.equal(whoisPrefix('', true), '');
  assert.equal(whoisPrefix(undefined, true), '');
});

test('reserved ranges are suppressed only on request', () => {
  assert.equal(whoisPrefix('RFC1918', true), '');
  assert.equal(whoisPrefix('DOD-NET', true), '');
  assert.equal(whoisPrefix('RFC1918', false), '[RFC1918]');
});

test('backbone ASNs, whois prefixes and 59.43/16 are highlighted like the CLI', () => {
  ['58807', '10099', '4809', '9929', '23764'].forEach((asnumber) => {
    assert.equal(classify({asnumber}, '1.2.3.4').backbone, true, asnumber);
  });
  ['CTG-CN', 'CNC-BACKBONE', 'CUG-BACKBONE-X', 'CMIN2-NET'].forEach((whois) => {
    assert.equal(classify({asnumber: '4837', whois}, '1.2.3.4').backbone, true, whois);
  });
  assert.equal(classify({asnumber: '4134'}, '59.43.80.1').backbone, true);
});

test('ordinary networks are not highlighted', () => {
  assert.equal(classify({asnumber: '4134', whois: 'CHINANET-BACKBONE'}, '202.97.1.1').backbone, false);
  assert.equal(classify({asnumber: '4837'}, '219.158.1.1').backbone, false);
  assert.equal(classify(null, '').backbone, false);
  assert.equal(classify({}, '159.43.1.1').backbone, false);
});
