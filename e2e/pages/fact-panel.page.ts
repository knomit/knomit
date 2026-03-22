import { type Page, type Locator } from '@playwright/test';

export class FactPanel {
  readonly page: Page;
  readonly title: Locator;
  readonly body: Locator;
  readonly meta: Locator;
  readonly typeBadge: Locator;
  readonly editor: Locator;
  readonly saveBtn: Locator;

  constructor(page: Page) {
    this.page = page;
    this.title = page.getByTestId('fact-title');
    this.body = page.getByTestId('fact-body');
    this.meta = page.getByTestId('fact-meta');
    this.typeBadge = page.getByTestId('fact-type-badge');
    this.editor = page.getByTestId('fact-editor');
    this.saveBtn = page.getByTestId('fact-save-btn');
  }

  async getTitle(): Promise<string> {
    return (await this.title.textContent()) || '';
  }

  async getBody(): Promise<string> {
    return (await this.body.textContent()) || '';
  }

  async editContent(content: string) {
    await this.editor.fill(content);
  }

  async save() {
    await this.saveBtn.click();
    await this.page.waitForResponse(
      resp => resp.url().includes('/fact') && resp.request().method() === 'PUT'
    );
  }
}
