

/**
 * CreateBlocked stands in for a create form the user cannot submit.
 *
 * The rail's `+` buttons are disabled on a read-only instance, so normally you
 * never reach a create form at all. One path gets past that: with zero
 * repositories the create form is the FALLBACK selection rather than something
 * you clicked, and it used to land on a fully live form whose submit would 403
 * with no warning.
 *
 * The only cause is the instance's own read-only mode. Creating a repository or
 * a lens is instance-scoped, so neither the browsed repo nor the history anchor
 * has any say in it (#324).
 */
export function CreateBlocked({ what }: { what: 'repository' | 'lens' }) {
  return (
    <div data-testid={`create-blocked-${what}`} style={{ maxWidth: 460, paddingTop: 30 }}>
      <h3 style={{ margin: 0, fontSize: 16 }}>Read-only</h3>
      <p style={{ fontSize: 12.5, color: '#888', lineHeight: 1.6, marginTop: 8 }}>
        No {what} can be created here: this instance is read-only.
      </p>
    </div>
  );
}
