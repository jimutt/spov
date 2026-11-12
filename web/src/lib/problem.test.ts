import { describe, expect, test } from 'vitest';
import { ApiError } from './api/client';
import { StreamProblem } from './api/sse';
import { describeProblem } from './problem';

describe('describeProblem', () => {
  test('a refused request shows the server title, detail and status', () => {
    const err = new ApiError(429, { type: 'about:blank', title: 'Too Many Requests', status: 429, detail: 'Try again in 2 seconds.' });
    expect(describeProblem(err)).toEqual({ title: 'Too Many Requests', detail: 'Try again in 2 seconds.', status: 429 });
  });

  // @decision(DL-133)
  test('a refused stream shows the server detail as a request does', () => {
    const err = new StreamProblem('Too Many Requests', {
      status: 429,
      problem: { type: 'about:blank', title: 'Too Many Requests', status: 429, detail: 'You have 20 live views open.' },
    });
    expect(describeProblem(err).detail).toBe('You have 20 live views open.');
  });

  test('anything else is its message', () => {
    expect(describeProblem(new Error('the stream broke'))).toEqual({ title: 'the stream broke', detail: undefined, status: undefined });
    expect(describeProblem(undefined).title).toBe('');
  });
});
