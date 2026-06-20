import { it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { TrailBreadcrumb } from './TrailBreadcrumb';

it('renders crumbs and fires jump/return', () => {
  const onJump = vi.fn(); const onReturnToNow = vi.fn();
  render(<TrailBreadcrumb
    trail={[
      { factPath: 'kb/a.md', asOf: { mode: 'live' } },
      { factPath: 'kb/b.md', asOf: { mode: 'scrubbed', commit: 'bbb1111' } },
    ]}
    onJump={onJump} onReturnToNow={onReturnToNow} />);
  fireEvent.click(screen.getByText('a'));
  expect(onJump).toHaveBeenCalledWith(0);
  fireEvent.click(screen.getByText(/return to now/i));
  expect(onReturnToNow).toHaveBeenCalled();
});
