import { test, expect } from '../../fixtures/knomit.js';

// The /api/v1/openapi.yaml endpoint has been removed in the HATEOAS redesign.
test.describe('API: OpenAPI', () => {
  test.skip('openapi.yaml endpoint is no longer registered in the HATEOAS router', async () => {
    // Previously: GET /api/v1/openapi.yaml — now removed.
  });
});
