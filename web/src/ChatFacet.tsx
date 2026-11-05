import type { ChatInfo } from './chatApi';

// The body of a chat target. Task 6.3 fills it in; until then it is the seat
// the shell mounts per live chat.
export function ChatFacet({ chat, facet }: {
  chat: ChatInfo;
  /** The tab on screen. */
  facet: 'chat' | 'settings';
  /** The chat is over; it leaves the list. */
  onGone: (id: string) => void;
}) {
  return <div data-testid="chat-facet" data-facet={facet}>{chat.model}</div>;
}
