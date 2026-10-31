package materialize

// Frames: a frame root's ancestors are planned by planner.ancestors
// (factset_plan.go), the user rulings of 2026-10-28 (16ccefe6 item 2 and the
// frame-parent ruling):
//   - the frame you are at is the CHILD and every frame it refs is a PARENT;
//     each ref is read at the version its ref_edges row pins (or the version
//     live at the anchor under unpin), so walking parents moves backward
//     through the temporal graph on its own; nothing computes when a frame
//     was created; a frame in another repo is a parent like any other ref
//     (cross_kb and the caller's reach apply) and the walk continues there;
//   - INCLUSION breadth-first from the starting frame over frame refs, closest
//     first against MaxNodes, any distance; ancestors are terminal (rendered
//     with their own data only, none of their other refs expanded);
//   - ORDER a depth-first POST-ORDER over frame refs in refs order: walk back
//     to the top, write top-down, so every parent precedes its child; a ref
//     to a frame already visited (the starting frame included) is a
//     back-reference, which settles a mutual pair (the frame reached first is
//     the child) and means the walk cannot loop; no cycle detection;
//   - in a FactSet, an ancestor already written by an earlier unit is a back
//     link, and one reserved for a later unit (or another unit's result) is a
//     forward link (ruling Q4: the reservation wins); a frame root reserves
//     nothing.
