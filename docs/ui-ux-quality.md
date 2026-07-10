# UI/UX Quality Bar

This project is a production operator tool for a physical CNC machine. The UI
standard is therefore closer to industrial control software than a prototype or
marketing site: it must be predictable, stable under live updates, accessible,
and explicit about machine-affecting actions.

## Source Baseline

These requirements are aligned with established external guidance:

- Nielsen Norman Group's usability heuristics: visibility of system status,
  match to the user's real world, user control, consistency, error prevention,
  recognition over recall, and minimalist design.
- W3C WCAG 2.2: visible focus, keyboard/pointer alternatives, minimum target
  sizing, understandable operation, and avoiding interaction modes that exclude
  users.
- GOV.UK and USWDS service principles: start with user needs, design with data,
  reduce unnecessary work, build for inclusion, and treat accessibility as core
  usability.
- Apple HIG and Material Design app-bar/layout guidance: bars are for current
  context, navigation, and high-frequency actions; overflow and adaptive
  layouts should be deliberate rather than accidental wrapping.
- IBM Carbon and comparable design systems: use explicit spacing and sizing
  scales, keep paired controls at consistent heights, and make form/component
  structure predictable.

References:

- <https://www.nngroup.com/articles/ten-usability-heuristics/>
- <https://www.w3.org/WAI/WCAG22/quickref/>
- <https://www.gov.uk/guidance/government-design-principles>
- <https://designsystem.digital.gov/design-principles/>
- <https://developer.apple.com/design/human-interface-guidelines/toolbars>
- <https://m3.material.io/components/app-bars/overview>
- <https://m3.material.io/foundations/layout/grids-spacing/spacing>
- <https://carbondesignsystem.com/components/text-input/usage/>
- <https://carbondesignsystem.com/elements/spacing/overview/>

## Non-Negotiable Requirements

### Task Model

- Start with the operator task, not the implementation object. A panel should be
  organized around actions like monitoring state, controlling motion, managing
  files, calibrating tools, or inspecting logs.
- Group actions by lifecycle and validity, not just by subject area. A
  state-gated confirmation action must not be presented as a peer of a general
  maintenance action just because both affect the same subsystem.
- When a modal machine state makes only one operator action valid, that action
  must become the sole primary action in its local group. Incompatible actions
  in the same surface must be disabled, hidden, or moved out of the immediate
  decision path until the modal state clears.
- Do not surface internal protocol concepts unless the operator needs them for
  diagnosis or the text is explicitly framed as diagnostics.
- Use CNC language and spatial mapping consistently. Motion controls, positions,
  origins, work offsets, tool length, probe state, and spindle state must match
  the machine's real behavior.

### Layout Stability

- A toolbar/header/action strip has a fixed row count for the lifetime of a
  viewport. Runtime state must not create a second row, change the bar height,
  push content down, or relocate controls.
- Dynamic text, validation, progress, and async status need reserved slots with
  fixed or bounded dimensions. If a value can grow, it truncates, scrolls inside
  its own region, or moves to a deliberate detail surface.
- Controls that are peers use a local sizing contract: same height, aligned
  baselines, stable widths for changing labels, and no native-widget mismatch
  between buttons, selects, number inputs, sliders, and toggles.
- Responsive behavior must be designed at breakpoints. If width is insufficient,
  move lower-priority controls into a stable overflow, drawer, popover, or task
  panel; never rely on opportunistic `flex-wrap` for production controls.

### Feedback And State

- Machine-action controls must show immediate local feedback, an in-progress
  state while the request is active, and a terminal result at the point of
  action.
- Claim success only after the same observable surface the operator would check
  confirms the effect. Otherwise show a concrete failure or leave the state
  pending/unknown.
- Live SSE/poll updates must not replace DOM nodes that own event handlers,
  focus, dirty input values, pointer capture, menu state, slider drag state, or
  request-pending state.
- Status should be close to the control or readout it explains, visually quieter
  than actions, and not duplicated unless transformed into task-specific
  context.
- Do not repeat a drawer, tab, popover, group, or section heading as the first
  title or field label inside that same visible region. The outer heading
  already labels the region; inner copy must name distinct subgroups or
  concrete controls. Exact or near-exact repeated labels are production defects.

### Accessibility And Input

- Keyboard operation is required for all non-canvas UI controls. Pointer-only,
  touch-only, hover-only, or drag-only operation is not acceptable unless an
  equivalent accessible control exists.
- Focus states must be visible and not hidden behind sticky panels or overlays.
- Dense desktop controls may be visually compact, but pointer targets must meet
  WCAG 2.2 minimum sizing and spacing. Touch/mobile or high-risk machine-action
  controls should use larger targets appropriate for deliberate operation.
- Labels, names, roles, disabled states, selected states, progress, and errors
  must be available to assistive technology where the control is not purely
  decorative.

### Density, Spacing, And Hierarchy

- Dense CNC screens are acceptable when they improve scanability, but density
  must be systematic. Use a small spacing scale based on 4px/8px increments
  instead of one-off margins.
- Whitespace encodes grouping: small gaps inside a group, larger gaps or rules
  between groups, and stable vertical rhythm even when a group has more controls
  than a neighbor.
- The primary action for a task must be visually primary. Secondary settings
  and diagnostic data must not compete with high-risk or session-defining
  controls.
- Avoid decorative structure that reduces clarity: nested cards, unrelated
  badges, ornamental panels, unnecessary icons, and visual noise around motion
  controls.

### Content And Error Handling

- Use concise operator-facing labels. Do not add explanatory product copy,
  marketing text, or speculative helper text inside production tools.
- Error messages must identify the concrete failing operation and, when known,
  the relevant machine or service state. They should not leak auth internals or
  invent recovery steps.
- Confirmation is required for destructive or hard-to-recover actions unless the
  action has a clear, immediate undo path.

## Implementation Checklist

Before a UI change is considered done:

- Verify the control remains wired after live updates, disabled/enabled cycles,
  and failed requests.
- Check loading, empty, success, failure, stale, disconnected, busy, and long
  label/value states.
- Check the narrowest supported desktop width and the mobile breakpoint.
- Check keyboard navigation, visible focus, and screen-reader names for changed
  controls.
- Check that machine-action controls provide local pending and terminal
  feedback without causing layout shift.
- Check that section, drawer, tab, and group labels are not repeated as local
  titles or field labels inside the same visible region.
- For canvas/3D work, verify the visual surface is nonblank, correctly framed,
  and still leaves controls readable at relevant viewport sizes.
