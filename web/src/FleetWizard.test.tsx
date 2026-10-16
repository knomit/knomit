import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FleetWizard } from './FleetWizard';
import { FacetLayoutProvider } from './facetLayout';
import { stepsFor, initialFleetWizardState, fleetWizardReducer } from './fleetWizardState';
import type { FleetStatus, FleetJoinCheck } from './api';
import { stubFleetFetch, problem } from './fleetTestFetch';

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
    expect(screen.getByTestId('fleet-hosting-urls')).toHaveTextContent('https://kb-01.acme.example:7443/git/fleet');
    expect(screen.getByTestId('wizard-step-1-summary')).toHaveTextContent('create');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    expect(screen.getByTestId('fleet-step-review')).toHaveTextContent('A fresh fleet root is created in /home/k/fleet on this machine');
    expect(screen.getByTestId('fleet-step-review')).toHaveTextContent('Members will join at https://kb-01.acme.example:7443/git/fleet.');
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    await waitFor(() => expect(onDone).toHaveBeenCalledWith({ kind: 'created' }));
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/create')?.body).toEqual({});
  });

  it('asks for the listener before Review when it is off, and a refusal shows its code with the create note', async () => {
    stubFleetFetch({ 'POST /api/v1/fleet/create': problem(422, 'listener_required', 'a fleet hosted here needs the mTLS listener: set [tls].addr') });
    mount({ ...standalone, host: { listener_on: false, external_addresses: [] } });
    expect(screen.getByTestId('fleet-start-cannot-host')).toHaveTextContent('This instance can create a fleet, but cannot host one yet');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    expect(screen.getByTestId('fleet-hosting-listener-off')).toHaveTextContent('The fleet listener is off on this instance');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    expect(await screen.findByTestId('fleet-wizard-error-code')).toHaveTextContent('listener_required');
    expect(screen.getByTestId('fleet-wizard-error')).toHaveTextContent('No fleet was created. You can change something and try again.');
    expect(screen.getByTestId('fleet-wizard-primary')).toHaveTextContent('Try again');
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
