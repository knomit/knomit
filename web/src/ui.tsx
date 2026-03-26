export function EmptyState({ message }: { message: string }) {
  return <div style={{ padding: 16, color: '#666', fontSize: 13 }}>{message}</div>;
}

export function LoadingSpinner() {
  return <div style={{ padding: 12, color: '#666', fontSize: 12, textAlign: 'center' }}>Loading...</div>;
}
