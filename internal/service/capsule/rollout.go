package capsule

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/rigdev/rig-go-api/api/v1/capsule"
	"github.com/rigdev/rig-go-api/model"
	"github.com/rigdev/rig/gen/go/proxy"
	"github.com/rigdev/rig/gen/go/rollout"
	"github.com/rigdev/rig/internal/gateway/cluster"
	"github.com/rigdev/rig/pkg/auth"
	"github.com/rigdev/rig/pkg/errors"
	"github.com/rigdev/rig/pkg/uuid"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Service) GetRollout(ctx context.Context, capsuleID uuid.UUID, rolloutID uint64) (*capsule.Rollout, error) {
	rc, rs, _, err := s.cr.GetRollout(ctx, capsuleID, rolloutID)
	return &capsule.Rollout{
		Config: rc,
		Status: rs.GetStatus(),
	}, err
}

func (s *Service) AbortRollout(ctx context.Context, capsuleID uuid.UUID, rolloutID uint64) error {
	_, rs, version, err := s.cr.GetRollout(ctx, capsuleID, rolloutID)
	if err != nil {
		return err
	}

	rs.Status.State = capsule.RolloutState_ROLLOUT_STATE_ABORTED
	rs.ScheduledAt = nil
	if err := s.cr.UpdateRolloutStatus(ctx, capsuleID, rolloutID, version, rs); err != nil {
		return err
	}

	return s.CreateEvent(ctx, capsuleID, rolloutID, "rollout aborted", &capsule.EventData{Kind: &capsule.EventData_Abort{}})
}

func (s *Service) newRollout(ctx context.Context, capsuleID uuid.UUID, cs []*capsule.Change) (uint64, error) {
	c, err := s.cr.Get(ctx, capsuleID)
	if err != nil {
		return 0, err
	}

	rc := &capsule.RolloutConfig{
		Replicas: 1,
	}

	if c.GetCurrentRollout() != 0 {
		pRC, ps, _, err := s.cr.GetRollout(ctx, capsuleID, c.GetCurrentRollout())
		if err != nil {
			return 0, err
		}

		if !isRolloutTerminated(ps) {
			return 0, errors.FailedPreconditionErrorf("rollout already in progress")
		}

		rc = pRC
	}

	now := time.Now()
	rc.Changes = cs
	rc.CreatedAt = timestamppb.New(now)
	if rc.CreatedBy, err = s.as.GetAuthor(ctx); err != nil {
		return 0, err
	}

	for _, c := range cs {
		switch v := c.GetField().(type) {
		case *capsule.Change_Replicas:
			rc.Replicas = v.Replicas
		case *capsule.Change_BuildId:
			rc.BuildId = v.BuildId
		case *capsule.Change_Network:
			rc.Network = v.Network
		case *capsule.Change_ContainerSettings:
			rc.ContainerSettings = v.ContainerSettings
		case *capsule.Change_AutoAddRigServiceAccounts:
			rc.AutoAddRigServiceAccounts = v.AutoAddRigServiceAccounts
		default:
			return 0, errors.InvalidArgumentErrorf("unhandled change field '%v'", reflect.TypeOf(v))
		}
	}

	// Validate the build exists.
	if _, err := s.cr.GetBuild(ctx, capsuleID, rc.GetBuildId()); err != nil {
		return 0, err
	}

	rs := &rollout.Status{
		Status: &capsule.RolloutStatus{
			State:     capsule.RolloutState_ROLLOUT_STATE_PENDING,
			UpdatedAt: timestamppb.New(now),
		},
		ScheduledAt: timestamppb.New(now),
	}

	rolloutID, err := s.cr.CreateRollout(ctx, capsuleID, rc, rs)
	if err != nil {
		return 0, err
	}

	c.CurrentRollout = rolloutID
	if err := s.cr.Update(ctx, c); err != nil {
		return 0, err
	}

	if err := s.queueRolloutJob(ctx, capsuleID, rolloutID, now); err != nil {
		return 0, err
	}

	return rolloutID, nil
}

func (s *Service) queueRolloutJob(ctx context.Context, capsuleID uuid.UUID, rolloutID uint64, ts time.Time) error {
	projectID, err := auth.GetProjectID(ctx)
	if err != nil {
		return err
	}

	s.q.AddJob(&rolloutJob{
		s:         s,
		projectID: projectID,
		capsuleID: capsuleID,
		rolloutID: rolloutID,
	}, ts)
	s.logger.Info("scheduled rollout job", zap.Time("scheduled_at", ts), zap.Stringer("capsule_id", capsuleID), zap.Uint64("rollout_id", rolloutID))

	return nil
}

type Job interface {
	Run(ctx context.Context) error
}

func (s *Service) run() {
	ctx := context.Background()

	for {
		// TODO: Move to rescheduling job (5s on error, 10m on success).
		if err := s.initJobs(ctx); err != nil {
			s.logger.Warn("failed to initialize jobs from repository", zap.Error(err))
			time.Sleep(5 * time.Second)
		} else {
			break
		}
	}

	const maxJobs = 10
	sem := semaphore.NewWeighted(maxJobs)

	for {
		if err := sem.Acquire(ctx, 1); err != nil {
			s.logger.Warn("error on semaphore acquire", zap.Error(err))
			return
		}

		s.logger.Info("waiting for job")
		job, err := s.q.Next(ctx, time.Now)
		if err != nil {
			s.logger.Warn("error getting next job", zap.Error(err))
			return
		}

		go func() {
			s.runJob(ctx, job)
			sem.Release(1)
		}()
	}
}

func (s *Service) initJobs(ctx context.Context) error {
	s.logger.Info("loading active rollouts from repository")

	it, err := s.cr.ActiveRollouts(ctx, &model.Pagination{})
	if err != nil {
		return err
	}

	for {
		ar, err := it.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}

		ctx := auth.WithProjectID(ctx, ar.ProjectID)
		if err := s.queueRolloutJob(ctx, ar.CapsuleID, ar.RolloutID, ar.ScheduledAt); err != nil {
			return err
		}
	}
}

func (s *Service) runJob(ctx context.Context, job Job) {
	if err := job.Run(ctx); err != nil {
		s.logger.Warn("error running job", zap.Error(err))
	}
}

type rolloutJob struct {
	s         *Service
	projectID uuid.UUID
	capsuleID uuid.UUID
	rolloutID uint64
}

func (j *rolloutJob) Run(ctx context.Context) error {
	ctx = auth.WithProjectID(ctx, j.projectID)

	logger := j.s.logger.With(
		zap.Stringer("project_id", j.projectID),
		zap.Stringer("capsule_id", j.capsuleID),
		zap.Uint64("rollout_id", j.rolloutID),
	)

	logger.Info("running rollout job")

	c, err := j.s.cr.Get(ctx, j.capsuleID)
	if err != nil {
		return err
	}

	rc, oldRS, version, err := j.s.cr.GetRollout(ctx, j.capsuleID, j.rolloutID)
	if err != nil {
		return err
	}

	rs := proto.Clone(oldRS).(*rollout.Status)

	err = j.run(ctx, c, rc, rs, version, logger)

	if proto.Equal(rs, oldRS) {
		rs.ScheduledAt = timestamppb.New(time.Now().Add(3 * time.Second))
		if err := j.s.cr.UpdateRolloutStatus(ctx, j.capsuleID, j.rolloutID, version, rs); err != nil {
			return err
		}
	} else {
		if errors.IsInvalidArgument(err) {
			rs.Status.State = capsule.RolloutState_ROLLOUT_STATE_FAILED
			rs.ScheduledAt = nil
		}

		if err == nil {
			err = j.updateContinue(ctx, rc, rs, version, logger)
		}

		if err != nil {
			j.updateError(ctx, rc, rs, version, err, logger)
		}
	}

	if rs.GetScheduledAt() != nil {
		if err := j.s.queueRolloutJob(ctx, j.capsuleID, j.rolloutID, rs.GetScheduledAt().AsTime()); err != nil {
			return err
		}
	}

	return err
}

func (j *rolloutJob) updateContinue(ctx context.Context, rc *capsule.RolloutConfig, rs *rollout.Status, version uint64, logger *zap.Logger) error {
	if isRolloutTerminated(rs) {
		rs.ScheduledAt = nil
	} else {
		rs.ScheduledAt = timestamppb.Now()
	}
	if err := j.s.cr.UpdateRolloutStatus(ctx, j.capsuleID, j.rolloutID, version, rs); err != nil {
		return err
	}

	return nil
}

func (j *rolloutJob) updateError(ctx context.Context, rc *capsule.RolloutConfig, rs *rollout.Status, version uint64, err error, logger *zap.Logger) {
	rs.ScheduledAt = timestamppb.New(time.Now().Add(3 * time.Second))
	rs.Status.Message = errors.MessageOf(err)
	if err := j.s.cr.UpdateRolloutStatus(ctx, j.capsuleID, j.rolloutID, version, rs); err != nil {
		logger.Info("error updating rollback on error", zap.Error(err))
	}

	if err := j.s.CreateEvent(ctx, j.capsuleID, j.rolloutID, errors.MessageOf(err), &capsule.EventData{Kind: &capsule.EventData_Error{Error: &capsule.ErrorEvent{}}}); err != nil {
		logger.Info("error creating error event", zap.Error(err))
	}
}

func (j *rolloutJob) run(
	ctx context.Context,
	c *capsule.Capsule,
	rc *capsule.RolloutConfig,
	rs *rollout.Status,
	version uint64,
	logger *zap.Logger,
) error {
	switch rs.GetStatus().GetState() {
	case capsule.RolloutState_ROLLOUT_STATE_PENDING:
		if err := j.s.CreateEvent(ctx, j.capsuleID, j.rolloutID, "new rollout initiated", &capsule.EventData{Kind: &capsule.EventData_Rollout{Rollout: &capsule.RolloutEvent{}}}); err != nil {
			return err
		}

		rs.Status.State = capsule.RolloutState_ROLLOUT_STATE_PREPARING
		rs.Status.Message = "preparing rollout"
		return nil

	case capsule.RolloutState_ROLLOUT_STATE_PREPARING:
		ccName := fmt.Sprint("rig-capsule-", c.GetName())
		if rc.GetAutoAddRigServiceAccounts() {
			if rs.GetRigServiceAccount().GetClientId() == "" {
				if err := j.s.CreateEvent(ctx, j.capsuleID, j.rolloutID, "creating service-account", &capsule.EventData{Kind: &capsule.EventData_Rollout{Rollout: &capsule.RolloutEvent{}}}); err != nil {
					return err
				}

				_, id, secret, err := j.s.as.CreateServiceAccount(ctx, ccName, true)
				if errors.IsAlreadyExists(err) {
					it, err := j.s.as.ListServiceAccounts(ctx)
					if err != nil {
						return err
					}

					defer it.Close()

					for {
						sa, err := it.Next()
						if err == io.EOF {
							break
						} else if err != nil {
							return err
						}

						if sa.GetServiceAccount().GetName() == ccName {
							if err := j.s.as.DeleteServiceAccount(ctx, uuid.UUID(sa.GetServiceAccountId())); err != nil {
								return err
							}
						}
					}

					_, id, secret, err = j.s.as.CreateServiceAccount(ctx, ccName, true)
					if err != nil {
						return err
					}
				} else if err != nil {
					return err
				}

				secretID := uuid.New()
				if err := j.s.sr.Create(ctx, secretID, []byte(secret)); err != nil {
					return err
				}

				rs.RigServiceAccount = &rollout.ServiceAccountCredentials{
					ClientId:        id,
					ClientSecretKey: secretID.String(),
				}
			}
		} else {
			if rs.GetRigServiceAccount().GetClientId() != "" {
				if err := j.s.CreateEvent(ctx, j.capsuleID, j.rolloutID, "deleting service-account", &capsule.EventData{Kind: &capsule.EventData_Rollout{Rollout: &capsule.RolloutEvent{}}}); err != nil {
					return err
				}

				sid := uuid.UUID(rs.GetRigServiceAccount().GetClientSecretKey())

				if err := j.s.sr.Delete(ctx, sid); errors.IsNotFound(err) {
				} else if err != nil {
					return err
				}

				it, err := j.s.as.ListServiceAccounts(ctx)
				if err != nil {
					return err
				}

				defer it.Close()

				for {
					cc, err := it.Next()
					if err == io.EOF {
						break
					} else if err != nil {
						return err
					}

					if cc.GetServiceAccount().GetName() == ccName {
						sid := uuid.UUID(cc.GetServiceAccountId())

						if err := j.s.as.DeleteServiceAccount(ctx, sid); err != nil {
							return err
						}
					}
				}

				rs.RigServiceAccount = nil
			}
		}

		rs.Status.State = capsule.RolloutState_ROLLOUT_STATE_DEPLOYING
		rs.Status.Message = "deploying rollout to cluster"
		return nil

	case capsule.RolloutState_ROLLOUT_STATE_DEPLOYING:
		b, err := j.s.cr.GetBuild(ctx, j.capsuleID, rc.GetBuildId())
		if errors.IsNotFound(err) {
			return errors.AbortedErrorf("build not available")
		} else if err != nil {
			return err
		}

		cc := &cluster.Capsule{
			CapsuleID:         j.capsuleID.String(),
			Image:             b.GetImage(),
			ContainerSettings: rc.GetContainerSettings(),
			BuildID:           rc.GetBuildId(),
			Replicas:          rc.GetReplicas(),
			Namespace:         j.projectID.String(),
			Network:           rc.GetNetwork(),
		}

		if cc.ContainerSettings == nil {
			cc.ContainerSettings = &capsule.ContainerSettings{}
		}
		if cc.ContainerSettings.EnvironmentVariables == nil {
			cc.ContainerSettings.EnvironmentVariables = map[string]string{}
		}

		cc.ContainerSettings.EnvironmentVariables["RIG_PROJECT_ID"] = j.projectID.String()
		if rc.GetAutoAddRigServiceAccounts() {
			sid := uuid.UUID(rs.GetRigServiceAccount().GetClientSecretKey())

			secretKey, err := j.s.sr.Get(ctx, sid)
			if err != nil {
				return err
			}

			cc.ContainerSettings.EnvironmentVariables["RIG_CLIENT_ID"] = rs.GetRigServiceAccount().GetClientId()
			cc.ContainerSettings.EnvironmentVariables["RIG_CLIENT_SECRET"] = string(secretKey)
		}

		sm, key := j.s.as.GetJWTMethod()
		switch sm.(type) {
		case *jwt.SigningMethodHMAC:
			cc.JWTMethod = &proxy.JWTMethod{Method: &proxy.JWTMethod_Secret{Secret: key}}
		default:
			cc.JWTMethod = &proxy.JWTMethod{Method: &proxy.JWTMethod_Certificate{Certificate: key}}
		}

		if err := j.s.CreateEvent(ctx, j.capsuleID, j.rolloutID, "configuring cluster resources", &capsule.EventData{Kind: &capsule.EventData_Rollout{Rollout: &capsule.RolloutEvent{}}}); err != nil {
			return err
		}

		// Upsert the capsule.
		if err := j.s.cg.UpsertCapsule(ctx, c.GetName(), cc); err != nil {
			return err
		}

		rs.Status.State = capsule.RolloutState_ROLLOUT_STATE_OBSERVING
		rs.Status.Message = "waiting for new instances"
		return nil

	case capsule.RolloutState_ROLLOUT_STATE_OBSERVING:
		it, _, err := j.s.cg.ListInstances(ctx, c.GetName())
		if err != nil {
			return err
		}

		c := 0
		for {
			i, err := it.Next()
			if err == io.EOF {
				break
			} else if err != nil {
				return err
			}

			if i.GetBuildId() != rc.GetBuildId() {
				return errors.UnavailableErrorf("instance '%s' is wrong build", i.GetInstanceId())
			}

			if i.GetState() != capsule.State_STATE_RUNNING {
				return errors.UnavailableErrorf("instance '%s' is running", i.GetInstanceId())
			}

			c++
		}

		if c < int(rc.GetReplicas()) {
			return errors.UnavailableErrorf("only %v instances running, should be '%v'", c, rc.GetReplicas())
		}

		if err := j.s.CreateEvent(ctx, j.capsuleID, j.rolloutID, "cluster resources created", &capsule.EventData{Kind: &capsule.EventData_Rollout{Rollout: &capsule.RolloutEvent{}}}); err != nil {
			return err
		}

		rs.Status.State = capsule.RolloutState_ROLLOUT_STATE_DONE
		rs.Status.Message = "rollout done"
		rs.ScheduledAt = nil
		return nil

		// Cleanup step, ensure we de-schedule terminated steps. Ideally, should not be needed.
	case
		capsule.RolloutState_ROLLOUT_STATE_ABORTED,
		capsule.RolloutState_ROLLOUT_STATE_DONE,
		capsule.RolloutState_ROLLOUT_STATE_FAILED:
		rs.ScheduledAt = nil
		return nil

	default:
		return errors.InvalidArgumentErrorf("invalid state %v", rs.GetStatus().GetState())
	}
}

func isRolloutTerminated(r *rollout.Status) bool {
	switch r.GetStatus().GetState() {
	case
		capsule.RolloutState_ROLLOUT_STATE_ABORTED,
		capsule.RolloutState_ROLLOUT_STATE_DONE,
		capsule.RolloutState_ROLLOUT_STATE_FAILED:
		return true

	default:
		return false
	}
}