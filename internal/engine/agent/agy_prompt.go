package agent

import (
	"fmt"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func buildAGYExecutionGoal(ticket *entity.Ticket) string {
	goal := ticket.Goal
	timeLimit := "调度器未设置固定时限"
	if ticket.ExecutionTimeout > 0 {
		timeLimit = fmt.Sprintf("调度器硬时限为 %s", ticket.ExecutionTimeout)
	}
	goal += fmt.Sprintf("\n\n【工单阶段边界】当前%s。若完整目标无法在本次时限内可靠完成，不要一次性铺开全部工作；先完成一个可独立验证、能在本阶段收敛的部分。阶段结束时明确写出：本阶段完成项、整体目标是否仍未完成、剩余阶段、下一阶段可直接执行的具体指令，以及涉及的文件或状态。不要把阶段完成表述成整体目标完成。若目标可以在本阶段完整完成，则正常完成并说明验证结果。", timeLimit)
	goal += "\n\n【执行边界】仅在当前工作区修改项目文件。工单明确授权的附件目录只能作为输入读取，不要修改附件或访问其他路径；如遇权限受限，如实说明受限步骤，不得伪报已完成。"
	if core.IsNixOS() {
		goal += "\n\n【运行环境与依赖说明】宿主机运行于 NixOS。若工单执行过程中缺少某些特定 CLI 工具（如编译器、分析工具、格式转换工具等），直接利用 NixOS 临时环境即时执行：nix shell nixpkgs#<pkg> -c <cmd>，无需向用户申请预装。"
	}
	if !ticket.AllowSensitive {
		goal += "\n\n【行为要求：当前不是核心搭档私聊。不要主动探测宿主机公网 IP，也不要在回复中输出本机公网 IP。该要求是行为指引，不代表操作系统权限隔离。】"
	}
	return goal
}
