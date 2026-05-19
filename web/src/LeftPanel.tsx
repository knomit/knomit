import type { Dispatch } from 'react';
import type { AppState, Action } from './state';
import type { NavRequest } from './useNavigationManager';
import { Library } from './Library';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
}

export function LeftPanel({ state, dispatch, navigate }: Props) {
  return <Library state={state} dispatch={dispatch} navigate={navigate} />;
}
