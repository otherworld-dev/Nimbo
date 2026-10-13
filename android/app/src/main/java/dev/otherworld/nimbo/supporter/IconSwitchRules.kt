/*
 * IconSwitchRules.kt — when the launcher icon may change, kept pure.
 *
 * Android closes a task when the launcher alias it was started from is
 * disabled, so the switch has to wait until nobody is in Nimbo's task. A
 * stopped Nimbo screen is not enough to know that: it is also stopped while
 * the file picker, a browser tab or a file viewer that Nimbo opened sits on
 * top of it, and while the phone is merely locked.
 */
package dev.otherworld.nimbo.supporter

/**
 * [startedScreens] is how many Nimbo screens are showing (started and not yet
 * stopped). [screenWentOff] is true when the last one stopped because the
 * display went off, so Nimbo is still what the user comes back to.
 * [otherAppOnTop] is [hasOtherAppOnTop].
 */
fun canSwitchIcon(startedScreens: Int, screenWentOff: Boolean, otherAppOnTop: Boolean): Boolean =
    startedScreens == 0 && !screenWentOff && !otherAppOnTop

/**
 * Whether another app's screen is on top of one of Nimbo's tasks: the user is
 * still in the task, and closing it would close what they are looking at.
 * [taskTops] is the package of each task's top screen, null for a task with no
 * screens left. Android can blank the package of a screen that isn't ours,
 * which still isn't ours.
 */
fun hasOtherAppOnTop(ownPackage: String, taskTops: List<String?>): Boolean =
    taskTops.any { it != null && it != ownPackage }
