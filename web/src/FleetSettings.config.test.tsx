import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FleetSettings } from './FleetSettings';
import type { FleetStatus } from './api';
import { stubFleetFetch, problem } from './fleetTestFetch';
import { hostPortOf } from './fleetText';

// PR7c: the fleet repository's configuration lives under /fleet (user
// rulings 2026-10-16). Move the fleet to a forge (the hosting master), Update
// access (a forge copy), Point at the new host after the fleet moved to a
// forge (the member's own credential), and Index → Rebuild. Every change is
// POST /fleet/..., never a generic repo route.

const FORGE = 'https://github.com/acme/fleet';
const hub: FleetStatus = {
  state: 'registered', agent_id: 'a3f9c0d12e7b', fleet_repo: 'fleet', master: true, local: true, hosting: 'none',
  record_state: 'active', public_key: 'ssh-ed25519 AAAAmaster', root_dir: '/home/k/fleet', root_fingerprint: 'SHA256:root',
  host: { listener_on: true, listening: true, external_addresses: ['https://kb-01.acme.example:7443'] },
  fleet_urls: ['https://kb-01.acme.example:7443/git/fleet'], follows_host: true,
};
const forgeMember: FleetStatus = {
  ...hub, agent_id: '5be2d8f4a901', master: false, hosting: FORGE, fleet_urls: [FORGE], host_record: [FORGE],
  host: { listener_on: false, external_addresses: [] },
};
const forgeMaster: FleetStatus = { ...forgeMember, agent_id: 'a3f9c0d12e7b', master: true, follows_host: true };
const PROBE_FORGE_ASKS = { body: { reachable: true, empty: false, auth_required: true, upstream_branch: 'main', branches: [] } };
const PROBE_FORGE_OK = { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] } };
const PROBE_PEER = { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'], fleet_peer: true } };

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function mount(fleet: FleetStatus) {
  const onChanged = vi.fn();
  render(<FleetSettings fleet={fleet} onChanged={onChanged} />);
  return onChanged;
}

describe('hostPortOf (#528 N6)', () => {
  // SABOTAGE: return the host alone (hostOf) -> the :19151 case is red.
  it('keeps a non-default port and drops the default one', () => {
    expect(hostPortOf('https://127.0.0.1:19151/git/fleet')).toBe('127.0.0.1:19151');
    expect(hostPortOf('https://u:tok@kb.example:443/git/fleet')).toBe('kb.example');
    expect(hostPortOf('ssh://git@github.com:2222/acme/fleet.git')).toBe('github.com:2222');
    expect(hostPortOf('git@github.com:acme/fleet.git')).toBe('github.com');
    expect(hostPortOf('file:///srv/fleet.git')).toBe('');
  });
});

describe('FleetSettings: the fleet repository configuration (PR7c)', () => {
  // The hosting master's Move: a forge URL, the Access step (the forge asks),
  // its credential checked, then a Review whose list says what members must
  // do, and POST /fleet/follow {url, credential}.
  // SABOTAGE: drop the member consequence item -> red; send the token as a
  // PUT /repos/fleet/origin -> no /fleet/follow call -> red.
  it('the hub master moves the fleet to a forge through the Access step and a Review naming the members\' step', async () => {
    let probes = 0;
    const { calls } = stubFleetFetch({
      'POST /api/v1/repos:probe-origin': () => (probes++ === 0 ? PROBE_FORGE_ASKS : PROBE_FORGE_OK),
      'POST /api/v1/fleet/follow': { body: { ...hub, hosting: FORGE } },
    });
    const onChanged = mount(hub);
    expect(screen.getByTestId('fleet-move-forge-hint')).toHaveTextContent('This instance stays the master');
    fireEvent.click(screen.getByTestId('fleet-move-forge'));
    expect(screen.getByTestId('fleet-follow-flow')).toHaveAttribute('data-mode', 'move');
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: FORGE } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-access-ask')).toHaveTextContent('github.com wants credentials');
    expect(screen.getByTestId('fleet-follow-next')).toBeDisabled();
    fireEvent.change(screen.getByTestId('fleet-follow-auth-method'), { target: { value: 'token' } });
    fireEvent.change(screen.getByTestId('fleet-follow-auth-token'), { target: { value: 'ghp_new' } });
    fireEvent.click(screen.getByTestId('fleet-follow-check'));
    expect(await screen.findByTestId('fleet-follow-access-ok')).toBeInTheDocument();
    expect(calls.filter(c => c.route === 'POST /api/v1/repos:probe-origin')[1].body).toEqual({ url: FORGE, auth_method: 'token', auth_token: 'ghp_new' });
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    const members = screen.getByTestId('fleet-move-members');
    expect(members).toHaveTextContent("Every member's pushes to this instance are refused from then on, until it points at " + FORGE + ' with its own credential');
    expect(members).toHaveTextContent('Its unmerged work is not lost');
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: FORGE, auth_method: 'token', auth_token: 'ghp_new' });
    expect(calls.map(c => c.route).filter(r => r.includes('/repos/fleet'))).toEqual([]);
  });

  // SABOTAGE: drop the peerForMove card -> the flow goes on to Review -> red.
  it('Move refuses an instance of this fleet: that is a master switch, not a forge', async () => {
    const { calls } = stubFleetFetch({ 'POST /api/v1/repos:probe-origin': PROBE_PEER });
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-move-forge'));
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: 'https://kb-02.acme.example:7443/git/fleet' } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-peer-not-forge')).toHaveTextContent('kb-02.acme.example:7443 is an instance of this fleet, not a forge');
    expect(screen.queryByTestId('fleet-follow-submit')).toBeNull();
    expect(calls.map(c => c.route)).not.toContain('POST /api/v1/fleet/follow');
  });

  // A forge copy: its stored method shown. Update access changes the
  // credential ONLY (#529 S3): it opens on Access with no URL field, the
  // stored method prefilled; unchanged it sends nothing (N-b); "none" resets
  // it to anonymous, sent with NO url (the server keeps the stored one). A
  // refusal shows the server's words and code. A member's URL change is
  // Point at the new host.
  // SABOTAGE: send the url with the credential -> red; drop the unchanged
  // guard -> Next is enabled with nothing changed -> red.
  it('a forge member updates its access: the credential only, no URL field, nothing sent while unchanged', async () => {
    const { calls, set } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'token' } },
      'POST /api/v1/repos:probe-origin': PROBE_FORGE_OK,
      'POST /api/v1/fleet/follow': problem(502, 'unreachable', 'the remote refused the credential'),
    });
    const onChanged = mount(forgeMember);
    expect(await screen.findByTestId('fleet-origin-access')).toHaveTextContent('token');
    expect(screen.getByTestId('fleet-point-at')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-point-at-hint')).toHaveTextContent('When the master moves the fleet');
    expect(screen.queryByTestId('fleet-move-forge')).toBeNull();
    expect(screen.getByTestId('fleet-update-access-hint')).toHaveTextContent('The forge URL stays as it is');
    fireEvent.click(screen.getByTestId('fleet-update-access'));
    expect(screen.getByTestId('fleet-follow-flow')).toHaveAttribute('data-mode', 'access');
    expect(screen.queryByTestId('fleet-follow-url')).toBeNull();
    expect(screen.queryByTestId('fleet-follow-step-origin')).toBeNull();
    expect(screen.getByTestId('fleet-follow-access')).toHaveTextContent('Stored now: token');
    expect(screen.getByTestId('fleet-follow-access-stored')).toHaveTextContent('This copy follows github.com');
    expect(screen.getByTestId('fleet-follow-auth-method')).toHaveValue('token');
    expect(screen.getByTestId('fleet-follow-unchanged')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-follow-next')).toBeDisabled();
    fireEvent.change(screen.getByTestId('fleet-follow-auth-method'), { target: { value: 'none' } });
    expect(screen.queryByTestId('fleet-follow-unchanged')).toBeNull();
    expect(screen.queryByTestId('fleet-follow-auth-token')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    expect(screen.getByTestId('fleet-follow-will')).toHaveTextContent('Nothing is pushed, and main, the host record and every member stay as they are');
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    expect(await screen.findByTestId('fleet-follow-error-code')).toHaveTextContent('unreachable');
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ auth_method: 'none' });
    expect(onChanged).not.toHaveBeenCalled();
    set('POST /api/v1/fleet/follow', { body: { ...forgeMember, change: 'credential' } });
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.map(c => c.route).filter(r => r.includes('/repos/fleet') && !r.startsWith('GET'))).toEqual([]);
  });

  // After Update access the Access row shows the NEW stored method: a
  // credential change leaves fleet_repo and hosting as they were, so the
  // origin read must not be keyed on them alone (found in the #529 browser
  // check: the row kept "none" after a rotation to a token).
  // While the re-read is in flight the previous read stays in use, so the
  // Hosting actions never blink out (CI caught a click on a vanished button).
  // SABOTAGE: drop the re-read after a finished flow -> the row keeps "token"
  // -> red; key the read on the revision -> the actions vanish while it is in
  // flight -> red.
  it('the Access row re-reads the stored method after Update access finishes', async () => {
    const { set } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'token' } },
      'POST /api/v1/repos:probe-origin': PROBE_FORGE_OK,
      'POST /api/v1/fleet/follow': { body: { ...forgeMember, change: 'credential' } },
    });
    // Hold every origin read after the first until released.
    let release = () => {};
    const gate = new Promise<void>(r => { release = r; });
    const inner = globalThis.fetch;
    let reads = 0;
    vi.stubGlobal('fetch', async (input: string | URL | Request, init?: RequestInit) => {
      if (String(input).includes('/repos/fleet/origin') && reads++ > 0) await gate;
      return inner(input, init);
    });
    const onChanged = mount(forgeMember);
    expect(await screen.findByTestId('fleet-origin-access')).toHaveTextContent('token');
    fireEvent.click(screen.getByTestId('fleet-update-access'));
    fireEvent.change(screen.getByTestId('fleet-follow-auth-method'), { target: { value: 'none' } });
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    set('GET /api/v1/repos/fleet/origin', { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'none' } });
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(screen.getByTestId('fleet-update-access')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-origin-access')).toHaveTextContent('token');
    release();
    await waitFor(() => expect(screen.getByTestId('fleet-origin-access')).toHaveTextContent('none'));
  });

  // The master on a forge (#529 S3): a URL change is a MOVE, through "Move
  // the fleet to another forge…", whose Review says members keep pushing to
  // the old forge until they follow. No Point at. Update access is the
  // credential only. A "move" to the forge it is already on is answered as
  // no move.
  // SABOTAGE: show Point at to the master on a forge -> red; use the hub's
  // member item for a forge move -> red.
  it('the master on a forge changes the URL only by moving the fleet to another forge', async () => {
    const OTHER = 'https://gitlab.example/acme/fleet';
    const { calls, set } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'token' } },
      'POST /api/v1/repos:probe-origin': PROBE_FORGE_OK,
      'POST /api/v1/fleet/follow': { body: { ...forgeMaster, hosting: OTHER, change: 'moved' } },
    });
    const onChanged = mount(forgeMaster);
    expect(await screen.findByTestId('fleet-origin-access')).toHaveTextContent('token');
    expect(screen.queryByTestId('fleet-point-at')).toBeNull();
    expect(screen.getByTestId('fleet-move-forge')).toHaveTextContent('Move the fleet to another forge…');
    expect(screen.getByTestId('fleet-update-access-hint')).toHaveTextContent('to change it, move the fleet to another forge');
    fireEvent.click(screen.getByTestId('fleet-move-forge'));
    expect(screen.getByTestId('fleet-follow-flow')).toHaveAttribute('data-mode', 'move');
    expect(screen.getByTestId('fleet-follow-url')).toHaveValue('');
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: OTHER } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    fireEvent.change(await screen.findByTestId('fleet-follow-auth-method'), { target: { value: 'none' } });
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    expect(screen.getByTestId('fleet-move-members')).toHaveTextContent(`Every member's copy keeps following ${FORGE} until it points at ${OTHER}`);
    expect(screen.getByTestId('fleet-move-members')).toHaveTextContent('its pushes go to the old forge, where nothing merges them');
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: OTHER, auth_method: 'none' });

    // The same forge it is on: the server ran no move, and the page says so.
    set('POST /api/v1/fleet/follow', { body: { ...forgeMaster, change: 'unchanged' } });
    onChanged.mockClear();
    fireEvent.click(screen.getByTestId('fleet-move-forge'));
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: FORGE } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    fireEvent.click(await screen.findByTestId('fleet-follow-next'));
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    expect(await screen.findByTestId('fleet-follow-not-moved')).toHaveTextContent('Nothing moved and nothing changed');
    expect(onChanged).not.toHaveBeenCalled();
    fireEvent.click(screen.getByTestId('fleet-follow-done'));
    expect(onChanged).toHaveBeenCalled();
  });

  // An interrupted move (#529 S4), from the server's interrupted_move: the
  // card offers the real repairs and never Point at, and never prefills
  // this instance's own hub URL.
  // SABOTAGE: show the follows-old-host card (Point at, prefilled from
  // host_record) for the master -> red.
  it('an interrupted move offers Host the fleet here, or moving back to the forge main names', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'token' } } });
    const hubURL = 'https://kb-01.acme.example:7443/git/fleet';
    const { unmount } = render(<FleetSettings fleet={{ ...forgeMaster, follows_host: false, host_record: [hubURL], fleet_urls: [hubURL], interrupted_move: { record_is_here: true } }} onChanged={vi.fn()} />);
    const card = await screen.findByTestId('fleet-move-interrupted');
    expect(card).toHaveTextContent('main still says this instance hosts the fleet');
    expect(card).toHaveTextContent('Host the fleet here (below)');
    expect(screen.queryByTestId('fleet-move-back')).toBeNull();
    expect(screen.queryByTestId('fleet-follows-old-host')).toBeNull();
    expect(screen.queryByTestId('fleet-point-at')).toBeNull();
    expect(screen.getByTestId('fleet-host-here')).toBeInTheDocument();
    unmount();

    const OLD = 'https://gitlab.example/acme/fleet';
    render(<FleetSettings fleet={{ ...forgeMaster, follows_host: false, host_record: [OLD], fleet_urls: [OLD], interrupted_move: { record_is_here: false, move_back_to: OLD } }} onChanged={vi.fn()} />);
    expect(await screen.findByTestId('fleet-move-interrupted')).toHaveTextContent(`still says the fleet is hosted at ${OLD}`);
    expect(screen.queryByTestId('fleet-point-at')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-move-back'));
    expect(screen.getByTestId('fleet-follow-flow')).toHaveAttribute('data-mode', 'move');
    expect(screen.getByTestId('fleet-follow-url')).toHaveValue(OLD);
  });

  // After an interrupted move the host record never changed and members follow
  // it, so neither repair screen may tell the operator to re-point members.
  // SABOTAGE: pass fromForge=fleet.hosting while interrupted (or drop the
  // interrupted variant of HostConfirm) -> the false member copy renders -> red.
  it('the repairs of an interrupted move tell the operator members have nothing to do', async () => {
    const OLD = 'https://gitlab.example/acme/fleet';
    stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'token' } },
      'POST /api/v1/repos:probe-origin': PROBE_FORGE_OK,
    });
    const { unmount } = render(<FleetSettings fleet={{ ...forgeMaster, follows_host: false, host_record: [OLD], fleet_urls: [OLD], interrupted_move: { record_is_here: false, move_back_to: OLD } }} onChanged={vi.fn()} />);
    fireEvent.click(await screen.findByTestId('fleet-move-back'));
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    fireEvent.change(await screen.findByTestId('fleet-follow-auth-method'), { target: { value: 'none' } });
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    const members = screen.getByTestId('fleet-move-members');
    expect(members).toHaveTextContent('Members have nothing to do');
    expect(members).not.toHaveTextContent('keeps following');
    expect(members).not.toHaveTextContent('until it points at');
    unmount();

    render(<FleetSettings fleet={{ ...forgeMaster, follows_host: false, host_record: ['https://kb-01.acme.example:7443/fleet'], interrupted_move: { record_is_here: true } }} onChanged={vi.fn()} />);
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    const confirm = await screen.findByTestId('fleet-host-confirm');
    expect(screen.getByTestId('fleet-host-members')).toHaveTextContent('Members have nothing to do');
    expect(confirm).not.toHaveTextContent('Tell every member the new URL');
    expect(confirm).not.toHaveTextContent('old master');
  });

  // The old host refused this copy's push because the fleet moved (#529 S5):
  // one plain sentence, never the raw refusal with "Retrying".
  // SABOTAGE: ignore last_error_fleet_moved -> the raw text and the retry
  // line render -> red.
  it('a member refused by the old host is told the fleet moved, not the raw push error', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: 'https://kb-01.acme.example:7443/git/fleet', branch: 'main', auth_method: 'cert' } } });
    const raw = 'Push: knomit: this copy of the fleet repository "fleet" has an origin; push your agent branch to the fleet\'s origin, not to this instance';
    mount({ ...forgeMember, hosting: 'https://kb-01.acme.example:7443/git/fleet', follows_host: false, last_error: raw, last_error_fleet_moved: true });
    expect(await screen.findByTestId('fleet-last-error-moved')).toHaveTextContent('The fleet has moved');
    expect(screen.getByTestId('fleet-last-error-moved')).toHaveTextContent('Point at the new host');
    expect(screen.queryByTestId('fleet-last-error')).toBeNull();
    expect(screen.queryByText(/Retrying on the next sync/)).toBeNull();
    expect(screen.queryByText(/push your agent branch/)).toBeNull();
    // #529 N-e: a stale copy names where main says the fleet is, not "the master".
    expect(screen.getByTestId('fleet-hosted-by')).toHaveTextContent("github.com — where the fleet's main says it is hosted now");
  });

  // After the hub moved to a forge: a member still on the old hub (a cert
  // origin) sees the stale card with the honest consequence, and Point at is
  // prefilled with the forge from main's host record and asks for its own
  // credential.
  // SABOTAGE: skip the Access step for a non-peer -> no credential fields ->
  // red.
  it('a member left on the old hub points at the forge with its own credential', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: 'https://kb-01.acme.example:7443/git/fleet', branch: 'main', auth_method: 'cert' } },
      'POST /api/v1/repos:probe-origin': PROBE_FORGE_OK,
      'POST /api/v1/fleet/follow': { body: forgeMember },
    });
    const onChanged = mount({ ...forgeMember, hosting: 'https://kb-01.acme.example:7443/git/fleet', follows_host: false });
    const card = await screen.findByTestId('fleet-follows-old-host');
    expect(card).toHaveTextContent('Until then the old host refuses this copy\'s pushes; nothing it wrote is lost');
    expect(screen.getByTestId('fleet-point-at-hint')).toHaveTextContent("On a forge it asks for this instance's own credential");
    fireEvent.click(screen.getByTestId('fleet-point-at'));
    expect(screen.getByTestId('fleet-follow-url')).toHaveValue(FORGE);
    expect(screen.getByTestId('fleet-follow-host-record')).toHaveTextContent(FORGE);
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-access')).toBeInTheDocument();
    fireEvent.change(screen.getByTestId('fleet-follow-auth-method'), { target: { value: 'basic' } });
    fireEvent.change(screen.getByTestId('fleet-follow-auth-user'), { target: { value: 'bravo' } });
    fireEvent.change(screen.getByTestId('fleet-follow-auth-token'), { target: { value: 'pw' } });
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    expect(screen.getByTestId('fleet-follow-will')).toHaveTextContent("carries this fleet's root");
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: FORGE, auth_method: 'basic', auth_token: 'bravo:pw' });
  });

  // Index → Rebuild: POST /fleet/index-rebuild, the started line; a refusal
  // under it. Over TCP (local false) there is no button (the TCP test in
  // FleetSettings.test.tsx asserts no action button at all).
  // SABOTAGE: call api.rebuild (the generic route) -> no /fleet call -> red.
  it('Index → Rebuild starts the fleet repository\'s rebuild through /fleet', async () => {
    const { calls, set } = stubFleetFetch({
      'POST /api/v1/fleet/index-rebuild': { status: 201, body: { id: 'rebuild-1', kind: 'index-rebuild', state: 'running', repo: 'fleet', branch: 'agent/a3f9c0d12e7b' } },
    });
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-rebuild'));
    expect(await screen.findByTestId('fleet-rebuild-status')).toHaveTextContent('Rebuild started');
    expect(calls.map(c => c.route)).toContain('POST /api/v1/fleet/index-rebuild');
    expect(calls.map(c => c.route).filter(r => r.includes('index-rebuilds'))).toEqual([]);
    set('POST /api/v1/fleet/index-rebuild', problem(409, 'not_registered', 'this instance is in no fleet'));
    fireEvent.click(screen.getByTestId('fleet-rebuild'));
    expect(await screen.findByTestId('fleet-rebuild-error-code')).toHaveTextContent('not_registered');
  });
});
