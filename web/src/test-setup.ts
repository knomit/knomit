import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

// @testing-library/react v16's auto-registered afterEach(cleanup) only runs
// when `afterEach` is a global function. This project's vitest config does
// not set `globals: true`, so we register cleanup explicitly.
afterEach(() => {
  cleanup();
});
