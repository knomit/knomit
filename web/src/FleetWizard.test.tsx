import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FleetWizard } from './FleetWizard';
import { FacetLayoutProvider } from './facetLayout';
import { stepsFor, initialFleetWizardState, fleetWizardReducer } from './fleetWizardState';
import type { FleetStatus, FleetJoinCheck } from './api';
import { stubFleetFetch, problem } from './fleetTestFetch';
import { HOST_HERE_COPY } from './fleetText';

// New fleet, at the fetch seam: the rail lists only the steps the state has
// established, each step sends exactly what its route takes, every refusal
// shows its code, and the bundle is sent, never shown.

const standalone: FleetStatus = {
  state: 'standalone', agent_id: '5be2d8f4a901', public_key: 'ssh-ed25519 AAAAjoiner', local: true, root_dir: '/home/k/fleet',
  host: { listener_on: true, external_addresses: ['https://kb-01.acme.example:7443'], would_serve: ['https://kb-01.acme.example:7443/git/fleet'] },
  fleet_urls: [],
};
const BUNDLE = '# knomit fleet bundle v1\nurl: https://kb-01.acme.example:7443/git/fleet\nagent: 5be2d8f4a901\nroot: SHA256:root\nserial: 2\n-----BEGIN CERTIFICATE-----\nSECRETPEMBODY\n-----END CERTIFICATE-----\n';
const MASTER_CHECK: FleetJoinCheck = {
  url: 'https://kb-01.acme.example:7443/git/fleet', hosting: 'master', host: 'kb-01.acme.example:7443',
  root_fingerprint: 'SHA256:root', agent_id: '5be2d8f4a901', auth_required: false,
};

afterEach(() => { vi.unstubAllGlobals(); });

const layout = { width: 220, onSplitterDown: () => {} };
function mount(fleet: FleetStatus = standalone) {
  const onDone = vi.fn();
  render(<FacetLayoutProvider value={layout as never}><FleetWizard fleet={fleet} onDone={onDone} onCancel={() => {}} /></FacetLayoutProvider>);
  return onDone;
}
const railSteps = () => screen.getAllByTestId(/^wizard-step-\d$/).map(s => s.children[1]?.textContent ?? '');
const pasteBundle = (text: string) => {
  Object.defineProperty(navigator, 'clipboard', { value: { readText: vi.fn().mockResolvedValue(text), writeText: vi.fn().mockResolvedValue(undefined) }, configurable: true });
  fireEvent.click(screen.getByTestId('fleet-bundle-paste'));
};

describe('fleetWizardState.stepsFor', () => {
  // SABOTAGE: list Access before a probe or a forge check -> red.
  it('lists only the steps the state has established', () => {
    const s = initialFleetWizardState;
    expect(stepsFor(s)).toEqual(['start', 'hosting', 'review']);
    expect(stepsFor({ ...s, hosting: 'forge' })).toEqual(['start', 'hosting']);
    expect(stepsFor({ ...s, hosting: 'forge', probe: { reachable: true, empty: false, auth_required: true, upstream_branch: 'main', branches: [] } }))
      .toEqual(['start', 'hosting', 'access', 'review']);
    expect(stepsFor({ ...s, choice: 'join' })).toEqual(['start', 'bundle']);
    expect(stepsFor({ ...s, choice: 'join', check: MASTER_CHECK })).toEqual(['start', 'bundle', 'review']);
    expect(stepsFor({ ...s, choice: 'join', check: { ...MASTER_CHECK, hosting: 'forge' } })).toEqual(['start', 'bundle', 'access', 'review']);
  });

  it('a new bundle drops its check, and a new credential the access confirmation', () => {
    const checked = fleetWizardReducer({ ...initialFleetWizardState, choice: 'join' }, { type: 'CHECKED', check: MASTER_CHECK });
    expect(checked.step).toBe('review');
    const reloaded = fleetWizardReducer(checked, { type: 'BUNDLE_LOADED', bundle: 'x', source: 'clipboard' });
    expect(reloaded.check).toBeNull();
    expect(reloaded.step).toBe('bundle');
    const ok = fleetWizardReducer(checked, { type: 'ACCESS_CHECKED' });
    expect(fleetWizardReducer(ok, { type: 'SET_TOKEN', token: 't' }).accessChecked).toBe(false);
  });
});

describe('FleetWizard: create', () => {
  it('this instance hosts it: Start · Hosting · Review, the server\'s URL, then POST /fleet/create {}', async () => {
    const { calls } = stubFleetFetch({ 'POST /api/v1/fleet/create': { status: 201, body: { ...standalone, state: 'registered', fleet_repo: 'fleet' } } });
    const onDone = mount();
    expect(railSteps()).toEqual(['Start', 'Hosting', 'Review']);
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    // Review B1: a fresh instance's listener is configured but cannot be
    // listening before the create installs its certificate: the URL is
    // named, and that nobody can join until the listener runs.
    const urls = screen.getByTestId('fleet-hosting-urls');
    expect(urls).toHaveTextContent('https://kb-01.acme.example:7443/git/fleet');
    expect(urls).toHaveAttribute('data-tone', 'ask');
    // PR8: the create starts the listener; no restart is asked for.
    expect(urls).toHaveTextContent('it needs the certificate the create installs, and the create starts it');
    expect(urls).not.toHaveTextContent('restart');
    expect(screen.getByTestId('wizard-step-1-summary')).toHaveTextContent('create');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    expect(screen.getByTestId('fleet-step-review')).toHaveTextContent('A fresh fleet root is created in /home/k/fleet on this machine');
    expect(screen.getByTestId('fleet-step-review')).toHaveTextContent('Members will join at https://kb-01.acme.example:7443/git/fleet: the create starts the fleet listener.');
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    await waitFor(() => expect(onDone).toHaveBeenCalledWith({ kind: 'created' }));
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/create')?.body).toEqual({});
  });

  // B1: only a listener that is UP is "Members will join at".
  // SABOTAGE: read listener_on as listening -> the good card with the
  // listener down (the test above) -> red; and the reverse here.
  it('with the fleet listener already running, Hosting and Review say members will join at the URL', () => {
    stubFleetFetch({});
    mount({ ...standalone, host: { ...standalone.host, listening: true } });
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    expect(screen.getByTestId('fleet-hosting-urls')).toHaveAttribute('data-tone', 'good');
    expect(screen.getByTestId('fleet-hosting-urls')).toHaveTextContent('Members will join at');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    expect(screen.getByTestId('fleet-step-review')).toHaveTextContent('Members will join at https://kb-01.acme.example:7443/git/fleet.');
    expect(screen.getByTestId('fleet-step-review')).not.toHaveTextContent('the create starts the fleet listener');
  });

  // Review nit 6: the wizard has its own `local` guard.
  // SABOTAGE: drop it -> Start renders over TCP -> red.
  it('over TCP (local false) shows no step, only that the desktop app is needed', () => {
    stubFleetFetch({});
    mount({ ...standalone, local: false });
    expect(screen.getByTestId('fleet-wizard-desktop-only')).toHaveAttribute('data-tone', 'ask');
    expect(screen.queryByTestId('fleet-step-start')).toBeNull();
    expect(screen.queryByTestId('fleet-wizard-next')).toBeNull();
  });

  it('asks for the listener before Review when it is off, and a refusal shows its code with the create note', async () => {
    stubFleetFetch({ 'POST /api/v1/fleet/create': problem(422, 'listener_required', 'a fleet hosted here needs the mTLS listener: set [tls].addr') });
    mount({ ...standalone, host: { listener_on: false, external_addresses: [] } });
    expect(screen.getByTestId('fleet-start-cannot-host')).toHaveTextContent('This instance can create a fleet, but cannot host one yet');
    expect(screen.getByTestId('fleet-start-cannot-host')).toHaveTextContent('Hosting needs the mTLS listener. Choose a forge on the next step, or set the Fleet port in Settings → Network first.');
    // The address is its own card, never folded into the listener's.
    expect(screen.getByTestId('fleet-start-needs-address')).toHaveTextContent(HOST_HERE_COPY.addressHeadline);
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    expect(screen.getByTestId('fleet-hosting-listener-off')).toHaveTextContent('The fleet listener is off on this instance');
    expect(screen.getByTestId('fleet-hosting-listener-off')).not.toHaveTextContent('external address');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    expect(await screen.findByTestId('fleet-wizard-error-code')).toHaveTextContent('listener_required');
    expect(screen.getByTestId('fleet-step-review')).toHaveTextContent(HOST_HERE_COPY.setFleetPort);
    expect(screen.getByTestId('fleet-wizard-error')).toHaveTextContent('No fleet was created. You can change something and try again.');
    expect(screen.getByTestId('fleet-wizard-primary')).toHaveTextContent('Try again');
  });

  // 2026-10-17 chicken-and-egg: a fresh instance with the Fleet port SET is
  // waiting for the certificate the create installs (no_certificate). That
  // must not read as "cannot host"; the create starts the listener.
  // SABOTAGE: gate the Start card on !listening -> the cannot-host card -> red.
  it('Fleet port set and waiting for its certificate: no cannot-host card, and the create goes ahead', async () => {
    const { calls } = stubFleetFetch({ 'POST /api/v1/fleet/create': { status: 201, body: { ...standalone, state: 'registered', fleet_repo: 'fleet' } } });
    const onDone = mount({ ...standalone, host: { ...standalone.host, listener: '0.0.0.0:19279', listening: false, listener_reason: 'no_certificate' } });
    expect(screen.queryByTestId('fleet-start-cannot-host')).toBeNull();
    expect(screen.queryByTestId('fleet-start-needs-address')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    expect(screen.getByTestId('fleet-hosting-urls')).toHaveTextContent('The fleet listener is not running yet: it needs the certificate the create installs, and the create starts it.');
    expect(screen.queryByTestId('fleet-hosting-listener-off')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    await waitFor(() => expect(onDone).toHaveBeenCalledWith({ kind: 'created' }));
    expect(calls.some(c => c.route === 'POST /api/v1/fleet/create')).toBe(true);
  });

  // An external address is genuinely required (addresses_required), and only
  // knomit.toml sets it: the wizard says so precisely, apart from the
  // listener, and never sends the user to Settings → Network for it.
  // SABOTAGE: fold the address back into the cannot-host card -> red.
  it('Fleet port set, no external address: a card that names knomit.toml and a restart, not the generic one', async () => {
    stubFleetFetch({ 'POST /api/v1/fleet/create': problem(422, 'addresses_required', 'a fleet hosted here needs external_addresses in knomit.toml') });
    mount({ ...standalone, host: { listener_on: true, listener: '0.0.0.0:19279', listening: false, listener_reason: 'no_certificate', external_addresses: [] } });
    expect(screen.queryByTestId('fleet-start-cannot-host')).toBeNull();
    const start = screen.getByTestId('fleet-start-needs-address');
    expect(start).toHaveTextContent(HOST_HERE_COPY.addressHeadline);
    expect(start).toHaveTextContent('external_addresses = ["https://<this computer\'s name>:19279"]');
    expect(start).toHaveTextContent('restart knomit (a reload does not apply it)');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    expect(screen.queryByTestId('fleet-hosting-listener-off')).toBeNull();
    expect(screen.getByTestId('fleet-hosting-needs-address')).toHaveTextContent(HOST_HERE_COPY.addressBody('0.0.0.0:19279'));
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    expect(await screen.findByTestId('fleet-wizard-error-code')).toHaveTextContent('addresses_required');
    const review = screen.getByTestId('fleet-step-review');
    expect(review).toHaveTextContent(HOST_HERE_COPY.addressBody('0.0.0.0:19279'));
    expect(review).not.toHaveTextContent('Settings → Network');
  });

  it('a forge: Connect probes, Access asks for the token, and the create sends the origin with it', async () => {
    const { calls } = stubFleetFetch({
      'POST /api/v1/repos:probe-origin': body => (body as { auth_token?: string }).auth_token
        ? { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] } }
        : { body: { reachable: true, empty: false, auth_required: true, upstream_branch: '', branches: [], detail: 'authentication required' } },
      'POST /api/v1/fleet/create': { status: 202, body: { ...standalone, state: 'registering', fleet_repo: 'fleet' } },
    });
    const onDone = mount();
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-hosting-forge'));
    expect(railSteps()).toEqual(['Start', 'Hosting']);
    expect(screen.queryByTestId('fleet-wizard-next')).toBeNull();
    fireEvent.change(screen.getByTestId('fleet-forge-url'), { target: { value: 'https://github.com/acme/fleet' } });
    fireEvent.click(screen.getByTestId('fleet-forge-connect'));
    await screen.findByTestId('fleet-step-access');
    expect(railSteps()).toEqual(['Start', 'Hosting', 'Access', 'Review']);
    expect(screen.getByTestId('fleet-access-ask')).toHaveTextContent('github.com wants credentials');
    expect(screen.getByTestId('fleet-wizard-next')).toBeDisabled();
    fireEvent.change(screen.getByTestId('fleet-auth-token'), { target: { value: 'ghp_tok' } });
    fireEvent.click(screen.getByTestId('fleet-access-check'));
    await screen.findByTestId('fleet-access-ok');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/create')?.body)
      .toEqual({ origin: { url: 'https://github.com/acme/fleet', auth_method: 'token', auth_token: 'ghp_tok' } });
  });
});

describe('FleetWizard: join', () => {
  it('Start shows this instance\'s id and key to send; Bundle checks it with join:check and never shows it', async () => {
    const { calls } = stubFleetFetch({
      'POST /api/v1/fleet/join:check': { body: MASTER_CHECK },
      'POST /api/v1/fleet/join': { body: { ...standalone, state: 'registered', fleet_repo: 'fleet', root_fingerprint: 'SHA256:root', record_state: 'pending' } },
    });
    const onDone = mount();
    fireEvent.click(screen.getByTestId('fleet-choice-join'));
    expect(screen.getByTestId('fleet-start-agent')).toHaveTextContent('5be2d8f4a901');
    expect(screen.getByTestId('fleet-start-key')).toHaveTextContent('ssh-ed25519 AAAAjoiner');
    expect(railSteps()).toEqual(['Start', 'Bundle']);
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    pasteBundle(BUNDLE);
    expect(await screen.findByTestId('fleet-bundle-loaded')).toHaveTextContent('Loaded from the clipboard. The bundle is sent with the join and never shown.');
    // Loaded and on screen: still not displayed (SABOTAGE: render s.bundle -> red).
    expect(document.body.textContent).not.toContain('SECRETPEMBODY');
    fireEvent.click(screen.getByTestId('fleet-bundle-check'));
    await screen.findByTestId('fleet-step-review');
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/join:check')?.body).toEqual({ bundle: BUNDLE });
    expect(railSteps()).toEqual(['Start', 'Bundle', 'Review']);
    // N6: the operator checks the host as well as the root.
    expect(screen.getByTestId('fleet-review-root')).toHaveTextContent('SHA256:root');
    expect(screen.getByTestId('fleet-review-host')).toHaveTextContent('kb-01.acme.example:7443');
    expect(document.body.textContent).not.toContain('SECRETPEMBODY');
    expect(document.body.textContent).not.toContain('knomit+');
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    await waitFor(() => expect(onDone).toHaveBeenCalledWith({ kind: 'joined', rootFingerprint: 'SHA256:root' }));
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/join')?.body).toEqual({ bundle: BUNDLE });
  });

  // Each join:check refusal gets its own card, with the server's text and code.
  // SABOTAGE: map unreachable to bundle_refused's card (or drop a row) -> red.
  it.each([
    ['unreachable', 502, "couldn't reach kb-01.acme.example:7443: dial tcp: connection refused", "Couldn't reach kb-01.acme.example:7443"],
    ['bundle_refused', 422, 'the fleet at kb-01.acme.example:7443 refused this bundle (tls: certificate revoked): ask the fleet\'s master for a new bundle', 'The fleet at kb-01.acme.example:7443 refused this bundle'],
    ['agent_mismatch', 422, 'the bundle\'s certificate is for agent "c71e4ab60d3f", not this instance\'s "5be2d8f4a901"', 'This bundle is for another instance'],
    ['key_mismatch', 422, 'the bundle\'s certificate is not for this instance\'s key', 'The bundle was issued for a different key'],
    ['bundle_invalid', 422, 'the bundle names no fleet repository URL', 'That is not a fleet bundle'],
  ])('join:check %s: its own card, the server text verbatim and the code', async (code, status, detail, headline) => {
    stubFleetFetch({ 'POST /api/v1/fleet/join:check': problem(status, code, detail) });
    mount();
    fireEvent.click(screen.getByTestId('fleet-choice-join'));
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    pasteBundle(BUNDLE);
    fireEvent.click(await screen.findByTestId('fleet-bundle-check'));
    const card = await screen.findByTestId('fleet-bundle-refused');
    expect(card).toHaveTextContent(headline);
    expect(card).toHaveTextContent(detail);
    expect(screen.getByTestId('fleet-bundle-code')).toHaveTextContent(code);
    expect(railSteps()).toEqual(['Start', 'Bundle']);
  });

  it('a fleet on a forge: Access with the joiner\'s own token, sent with the join', async () => {
    const forge = { ...MASTER_CHECK, hosting: 'forge' as const, url: 'https://github.com/acme/fleet.git', host: 'github.com', auth_required: true };
    const { calls } = stubFleetFetch({
      'POST /api/v1/fleet/join:check': body => ({ body: (body as { auth_token?: string }).auth_token ? { ...forge, auth_required: false } : forge }),
      'POST /api/v1/fleet/join': problem(409, 'join_refused', 'the fleet did not take this instance\'s record'),
    });
    mount();
    fireEvent.click(screen.getByTestId('fleet-choice-join'));
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    pasteBundle(BUNDLE);
    fireEvent.click(await screen.findByTestId('fleet-bundle-check'));
    await screen.findByTestId('fleet-step-access');
    expect(railSteps()).toEqual(['Start', 'Bundle', 'Access', 'Review']);
    fireEvent.change(screen.getByTestId('fleet-auth-token'), { target: { value: 'my-own' } });
    fireEvent.click(screen.getByTestId('fleet-access-check'));
    await screen.findByTestId('fleet-access-ok');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    expect(await screen.findByTestId('fleet-wizard-error-code')).toHaveTextContent('join_refused');
    expect(screen.getByTestId('fleet-wizard-error')).toHaveTextContent('Nothing was kept. You can try again once the master’s next round has published the certificate.');
    expect(calls.filter(c => c.route === 'POST /api/v1/fleet/join')[0].body).toEqual({ bundle: BUNDLE, auth_method: 'token', auth_token: 'my-own' });
  });
});
