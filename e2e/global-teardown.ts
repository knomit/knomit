import { cleanupFromState } from './helpers/cleanup.js';

function globalTeardown() {
  cleanupFromState();
}

export default globalTeardown;
