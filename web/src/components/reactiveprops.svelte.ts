/**
 * reactiveProps is a `$state` object a mounted test passes as a
 * component's props, so a test can change one after the mount. Test
 * furniture: nothing on a page imports it.
 */
export function reactiveProps<T extends object>(initial: T): T {
  const props = $state(initial);
  return props;
}
